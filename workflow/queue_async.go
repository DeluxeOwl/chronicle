package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
)

// AsyncQueue is a SQL-backed TaskQueue populated by an AsyncProjection.
// It watches the global event stream for workflow events and builds a
// ready_tasks table accordingly. This enables task discovery for
// non-transactional event logs (in-memory, NATS, DynamoDB, etc.) where
// we can't atomically write events + enqueue tasks.
//
// Trade-off: Task discovery is eventually consistent (delayed by the
// projection's pollInterval). Execution is still correct because the
// event log's optimistic concurrency prevents duplicate step execution.
//
// AsyncQueue implements both TaskQueue (for Poll/Complete/Fail/ExtendLease)
// and event.AsyncProjection (for the projection runner to drive).
type AsyncQueue struct {
	db            *sql.DB
	nowFunc       func() time.Time
	workerID      string
	leaseDuration time.Duration
}

// AsyncQueueOption configures an AsyncQueue.
type AsyncQueueOption func(*AsyncQueue)

// WithAsyncQueueNowFunc sets a custom clock for the queue.
func WithAsyncQueueNowFunc(fn func() time.Time) AsyncQueueOption {
	return func(q *AsyncQueue) {
		q.nowFunc = fn
	}
}

// WithAsyncQueueLeaseDuration sets the default lease duration when claiming a task.
func WithAsyncQueueLeaseDuration(d time.Duration) AsyncQueueOption {
	return func(q *AsyncQueue) {
		q.leaseDuration = d
	}
}

// WithAsyncQueueWorkerID sets a custom worker ID for claiming tasks.
func WithAsyncQueueWorkerID(id string) AsyncQueueOption {
	return func(q *AsyncQueue) {
		q.workerID = id
	}
}

// NewAsyncQueue creates a SQL-backed task queue populated by an async projection.
// It creates the necessary tables if they don't already exist.
func NewAsyncQueue(db *sql.DB, opts ...AsyncQueueOption) (*AsyncQueue, error) {
	q := &AsyncQueue{
		db:            db,
		nowFunc:       time.Now,
		workerID:      fmt.Sprintf("worker-%d", time.Now().UnixNano()),
		leaseDuration: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(q)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_ready_tasks (
			instance_id TEXT PRIMARY KEY,
			workflow_name TEXT NOT NULL,
			run_after_ns INTEGER NOT NULL DEFAULT 0,
			claimed_by TEXT,
			claimed_until_ns INTEGER
		)
	`); err != nil {
		return nil, fmt.Errorf("create ready_tasks table: %w", err)
	}

	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_workflow_ready_tasks_poll
		ON workflow_ready_tasks (run_after_ns)
		WHERE claimed_by IS NULL
	`); err != nil {
		return nil, fmt.Errorf("create poll index: %w", err)
	}

	// Tables for persistent event waiting.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_waiting_events (
			instance_id TEXT NOT NULL,
			event_name TEXT NOT NULL,
			workflow_name TEXT NOT NULL,
			step_index INTEGER NOT NULL DEFAULT 0,
			deadline_ns INTEGER,
			PRIMARY KEY (instance_id, event_name)
		)
	`); err != nil {
		return nil, fmt.Errorf("create waiting_events table: %w", err)
	}

	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_workflow_waiting_events_name
		ON workflow_waiting_events (event_name)
	`); err != nil {
		return nil, fmt.Errorf("create waiting_events index: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_published_events (
			event_name TEXT PRIMARY KEY,
			payload TEXT NOT NULL
		)
	`); err != nil {
		return nil, fmt.Errorf("create published_events table: %w", err)
	}

	return q, nil
}

// --- AsyncProjection implementation ---

// workflowEventNames lists all workflow event names the projection cares about.
var workflowEventNames = map[string]bool{
	"workflow/started":        true,
	"workflow/step_completed": true,
	"workflow/completed":      true,
	"workflow/failed":         true,
	"workflow/retried":        true,
	"workflow/waiting":        true,
	"workflow/event_received": true,
	"workflow/cancelled":      true,
}

// MatchesEvent returns true if the event is a workflow event that should
// be processed by this projection.
func (q *AsyncQueue) MatchesEvent(eventName string) bool {
	return workflowEventNames[eventName]
}

// Handle processes a single global event record and updates the ready_tasks
// table accordingly. This is called by the AsyncProjectionRunner.
//
//nolint:gocognit,funlen
func (q *AsyncQueue) Handle(ctx context.Context, rec *event.GlobalRecord) error {
	instanceID := InstanceID(rec.LogID())

	switch rec.EventName() {
	case "workflow/started":
		var e workflowStarted
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowStarted: %w", err)
		}
		return q.upsertTask(ctx, InstanceID(e.InstanceID), e.WorkflowName, q.nowFunc())

	case "workflow/step_completed":
		var e stepCompleted
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal stepCompleted: %w", err)
		}
		// Check if this is a sleep step.
		var sr sleepResult
		if err := json.Unmarshal(e.Result, &sr); err == nil && !sr.WakeAt.IsZero() {
			// We need the workflow name — get it from the existing task or use the logID.
			workflowName := q.getWorkflowName(instanceID)
			if workflowName == "" {
				// Fallback: the workflow name isn't available; use the logID as a
				// best-effort. The worker will look it up by name from the registry.
				workflowName = string(rec.LogID())
			}
			return q.upsertTask(ctx, instanceID, workflowName, sr.WakeAt)
		}
		return nil

	case "workflow/retried":
		var e workflowRetried
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowRetried: %w", err)
		}
		workflowName := q.getWorkflowName(instanceID)
		if workflowName == "" {
			workflowName = string(rec.LogID())
		}
		return q.upsertTask(ctx, instanceID, workflowName, e.NextRunAfter)

	case "workflow/waiting":
		var e workflowWaiting
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowWaiting: %w", err)
		}
		workflowName := q.getWorkflowName(instanceID)
		if workflowName == "" {
			workflowName = string(rec.LogID())
		}
		// Record the waiter.
		if err := q.insertWaitingEvent(ctx, instanceID, workflowName, e.AwaitingEvent, e.StepIndex, e.Deadline); err != nil {
			return err
		}
		// Check if event was already published (pre-publish case).
		if q.isEventPublished(e.AwaitingEvent) {
			return q.upsertTask(ctx, instanceID, workflowName, q.nowFunc())
		}
		if !e.Deadline.IsZero() {
			return q.upsertTask(ctx, instanceID, workflowName, e.Deadline)
		}
		return nil

	case "workflow/event_received":
		var e workflowEventReceived
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowEventReceived: %w", err)
		}
		q.deleteWaitingEvent(instanceID, e.ReceivedEvent)
		workflowName := q.getWorkflowName(instanceID)
		if workflowName == "" {
			workflowName = string(rec.LogID())
		}
		return q.upsertTask(ctx, instanceID, workflowName, q.nowFunc())

	case "workflow/completed":
		q.deleteAllWaitingEvents(instanceID)
		return q.deleteTask(ctx, instanceID)

	case "workflow/failed":
		q.deleteAllWaitingEvents(instanceID)
		return q.deleteTask(ctx, instanceID)

	case "workflow/cancelled":
		q.deleteAllWaitingEvents(instanceID)
		return q.deleteTask(ctx, instanceID)

	default:
		return nil
	}
}

// --- TaskQueue implementation ---

// Enqueue inserts a task into the ready_tasks table.
func (q *AsyncQueue) Enqueue(_ context.Context, task QueuedTask) error {
	runAfterNs := int64(0)
	if !task.RunAfter.IsZero() {
		runAfterNs = task.RunAfter.UnixNano()
	}

	_, err := q.db.Exec(`
		INSERT INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns, claimed_by, claimed_until_ns)
		VALUES (?, ?, ?, NULL, NULL)
		ON CONFLICT(instance_id) DO UPDATE SET
			workflow_name = excluded.workflow_name,
			run_after_ns = MIN(workflow_ready_tasks.run_after_ns, excluded.run_after_ns),
			claimed_by = NULL,
			claimed_until_ns = NULL
	`, string(task.InstanceID), task.WorkflowName, runAfterNs)
	if err != nil {
		return fmt.Errorf("async queue enqueue: %w", err)
	}
	return nil
}

// Poll finds the next ready task, atomically claims it, and returns it.
func (q *AsyncQueue) Poll(ctx context.Context) (*QueuedTask, error) {
	now := q.nowFunc()
	nowNs := now.UnixNano()
	claimedUntilNs := now.Add(q.leaseDuration).UnixNano()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("async queue poll begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // This is fine.

	var instanceID, workflowName string
	var runAfterNs int64

	err = tx.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, run_after_ns
		FROM workflow_ready_tasks
		WHERE run_after_ns <= ?
		AND (claimed_by IS NULL OR claimed_until_ns <= ?)
		ORDER BY run_after_ns
		LIMIT 1
	`, nowNs, nowNs).Scan(&instanceID, &workflowName, &runAfterNs)

	if errors.Is(err, sql.ErrNoRows) {
		//nolint:nilnil // This is fine.
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("async queue poll select: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_ready_tasks
		SET claimed_by = ?, claimed_until_ns = ?
		WHERE instance_id = ?
		AND (claimed_by IS NULL OR claimed_until_ns <= ?)
	`, q.workerID, claimedUntilNs, instanceID, nowNs)
	if err != nil {
		return nil, fmt.Errorf("async queue poll claim: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("async queue poll rows affected: %w", err)
	}

	if affected == 0 {
		//nolint:nilnil // This is fine.
		return nil, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("async queue poll commit: %w", err)
	}

	task := &QueuedTask{
		InstanceID:   InstanceID(instanceID),
		WorkflowName: workflowName,
	}
	if runAfterNs > 0 {
		task.RunAfter = time.Unix(0, runAfterNs)
	}
	return task, nil
}

// Complete deletes the task claimed by this worker.
func (q *AsyncQueue) Complete(_ context.Context, instanceID InstanceID) error {
	_, err := q.db.Exec(`
		DELETE FROM workflow_ready_tasks
		WHERE instance_id = ? AND claimed_by = ?
	`, string(instanceID), q.workerID)
	if err != nil {
		return fmt.Errorf("async queue complete: %w", err)
	}
	return nil
}

// Fail deletes the task claimed by this worker.
func (q *AsyncQueue) Fail(_ context.Context, instanceID InstanceID) error {
	_, err := q.db.Exec(`
		DELETE FROM workflow_ready_tasks
		WHERE instance_id = ? AND claimed_by = ?
	`, string(instanceID), q.workerID)
	if err != nil {
		return fmt.Errorf("async queue fail: %w", err)
	}
	return nil
}

// ExtendLease extends the claim timeout for a running task.
func (q *AsyncQueue) ExtendLease(
	_ context.Context,
	instanceID InstanceID,
	lease time.Duration,
) error {
	newDeadline := q.nowFunc().Add(lease).UnixNano()
	_, err := q.db.Exec(`
		UPDATE workflow_ready_tasks
		SET claimed_until_ns = ?
		WHERE instance_id = ? AND claimed_by = ?
	`, newDeadline, string(instanceID), q.workerID)
	if err != nil {
		return fmt.Errorf("async queue extend lease: %w", err)
	}
	return nil
}

// Len returns the total number of tasks in the queue (for testing).
func (q *AsyncQueue) Len() int {
	var count int
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM workflow_ready_tasks`).Scan(&count)
	return count
}

// --- Internal helpers ---

// upsertTask inserts or replaces a task in the ready_tasks table.
func (q *AsyncQueue) upsertTask(
	_ context.Context,
	instanceID InstanceID,
	workflowName string,
	runAfter time.Time,
) error {
	runAfterNs := int64(0)
	if !runAfter.IsZero() {
		runAfterNs = runAfter.UnixNano()
	}

	_, err := q.db.Exec(`
		INSERT OR REPLACE INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns, claimed_by, claimed_until_ns)
		VALUES (?, ?, ?, NULL, NULL)
	`, string(instanceID), workflowName, runAfterNs)
	if err != nil {
		return fmt.Errorf("async queue upsert task: %w", err)
	}
	return nil
}

// deleteTask removes a task from the ready_tasks table.
func (q *AsyncQueue) deleteTask(_ context.Context, instanceID InstanceID) error {
	_, err := q.db.Exec(`
		DELETE FROM workflow_ready_tasks WHERE instance_id = ?
	`, string(instanceID))
	if err != nil {
		return fmt.Errorf("async queue delete task: %w", err)
	}
	return nil
}

// getWorkflowName retrieves the workflow_name from the ready_tasks table for
// an existing task. Returns empty string if not found.
func (q *AsyncQueue) getWorkflowName(instanceID InstanceID) string {
	var name string
	_ = q.db.QueryRow(`
		SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?
	`, string(instanceID)).Scan(&name)
	return name
}

// insertWaitingEvent records a workflow waiting for an event.
func (q *AsyncQueue) insertWaitingEvent(
	_ context.Context,
	instanceID InstanceID,
	workflowName string,
	eventName string,
	stepIndex int,
	deadline time.Time,
) error {
	var deadlineNs *int64
	if !deadline.IsZero() {
		v := deadline.UnixNano()
		deadlineNs = &v
	}

	_, err := q.db.Exec(`
		INSERT OR REPLACE INTO workflow_waiting_events
			(instance_id, event_name, workflow_name, step_index, deadline_ns)
		VALUES (?, ?, ?, ?, ?)
	`, string(instanceID), eventName, workflowName, stepIndex, deadlineNs)
	if err != nil {
		return fmt.Errorf("async queue insert waiting event: %w", err)
	}
	return nil
}

// isEventPublished checks if an event has already been published.
func (q *AsyncQueue) isEventPublished(eventName string) bool {
	var count int
	err := q.db.QueryRow(`
		SELECT COUNT(*) FROM workflow_published_events WHERE event_name = ?
	`, eventName).Scan(&count)
	return err == nil && count > 0
}

// deleteWaitingEvent removes a specific waiter.
func (q *AsyncQueue) deleteWaitingEvent(instanceID InstanceID, eventName string) {
	_, _ = q.db.Exec(`
		DELETE FROM workflow_waiting_events
		WHERE instance_id = ? AND event_name = ?
	`, string(instanceID), eventName)
}

// deleteAllWaitingEvents removes all waiters for an instance.
func (q *AsyncQueue) deleteAllWaitingEvents(instanceID InstanceID) {
	_, _ = q.db.Exec(`
		DELETE FROM workflow_waiting_events WHERE instance_id = ?
	`, string(instanceID))
}

// Compile-time interface checks.
var (
	_ TaskQueue             = (*AsyncQueue)(nil)
	_ event.AsyncProjection = (*AsyncQueue)(nil)
)
