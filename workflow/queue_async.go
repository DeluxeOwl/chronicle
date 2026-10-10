package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
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
// Task discovery is eventually consistent. Receipts protect queue ownership,
// but neither receipts nor event-log concurrency make external effects exactly
// once. Steps that cause external effects must tolerate repeated executions.
//
// AsyncQueue implements both TaskQueue (for Poll/Complete/Fail/ExtendLease)
// and event.AsyncProjection (for the projection runner to drive).
type AsyncQueue struct {
	db            *sql.DB
	nowFunc       func() time.Time
	workerID      string
	leaseDuration time.Duration
	legacyClaims  legacyQueueClaims
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
func NewAsyncQueue(ctx context.Context, db *sql.DB, opts ...AsyncQueueOption) (*AsyncQueue, error) {
	q := &AsyncQueue{
		db:            db,
		nowFunc:       time.Now,
		workerID:      fmt.Sprintf("worker-%d", time.Now().UnixNano()),
		leaseDuration: defaultQueueLeaseDuration,
		legacyClaims:  newLegacyQueueClaims(),
	}
	for _, opt := range opts {
		opt(q)
	}

	if err := validateLease(q.leaseDuration); err != nil {
		return nil, err
	}
	if err := ensureReadyTasksSchema(ctx, db); err != nil {
		return nil, err
	}

	// Tables for persistent event waiting.
	if _, err := db.ExecContext(ctx, `
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

	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_workflow_waiting_events_name
		ON workflow_waiting_events (event_name)
	`); err != nil {
		return nil, fmt.Errorf("create waiting_events index: %w", err)
	}

	if err := ensurePublishedEventsSchema(ctx, db); err != nil {
		return nil, err
	}

	return q, nil
}

// --- AsyncProjection implementation ---

// workflowEventNames lists all workflow event names the projection cares about.
var workflowEventNames = map[string]bool{
	eventNameWorkflowStarted:       true,
	eventNameStepCompleted:         true,
	eventNameWorkflowCompleted:     true,
	eventNameWorkflowFailed:        true,
	eventNameWorkflowRetried:       true,
	eventNameWorkflowWaiting:       true,
	eventNameWorkflowEventReceived: true,
	eventNameWorkflowCancelled:     true,
}

// MatchesEvent returns true if the event is a workflow event that should
// be processed by this projection.
func (q *AsyncQueue) MatchesEvent(eventName string) bool {
	return workflowEventNames[eventName]
}

// Handle processes a single global event record and updates the ready_tasks
// table accordingly. This is called by the AsyncProjectionRunner.
//
//nolint:funlen,gocognit // One projection dispatch keeps event-to-task mappings together.
func (q *AsyncQueue) Handle(ctx context.Context, rec *event.GlobalRecord) error {
	instanceID := InstanceID(rec.LogID())

	switch rec.EventName() {
	case eventNameWorkflowStarted:
		var e workflowStarted
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowStarted: %w", err)
		}
		return q.upsertTask(ctx, InstanceID(e.InstanceID), e.WorkflowName, time.Time{})

	case eventNameStepCompleted:
		var e stepCompleted
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal stepCompleted: %w", err)
		}
		// Check if this is a sleep step.
		var sr sleepResult
		if (e.Kind == "sleep" || e.Kind == "") && json.Unmarshal(e.Result, &sr) == nil &&
			!sr.WakeAt.IsZero() {
			workflowName := q.resolveWorkflowName(ctx, e.WorkflowName, instanceID)
			return q.upsertTask(ctx, instanceID, workflowName, sr.WakeAt)
		}
		return nil

	case eventNameWorkflowRetried:
		var e workflowRetried
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowRetried: %w", err)
		}
		workflowName := q.resolveWorkflowName(ctx, e.WorkflowName, instanceID)
		return q.upsertTask(ctx, instanceID, workflowName, e.NextRunAfter)

	case eventNameWorkflowWaiting:
		var e workflowWaiting
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowWaiting: %w", err)
		}
		workflowName := q.resolveWorkflowName(ctx, q.getWorkflowName(ctx, instanceID), instanceID)
		// Record the waiter.
		if err := q.insertWaitingEvent(ctx, instanceID, workflowName, e.AwaitingEvent, e.StepIndex, e.Deadline); err != nil {
			return err
		}
		// Check if event was already published (pre-publish case).
		published, err := q.isEventPublished(ctx, e.AwaitingEvent)
		if err != nil {
			return err
		}
		if published {
			return q.upsertTask(ctx, instanceID, workflowName, time.Time{})
		}
		if !e.Deadline.IsZero() {
			return q.upsertTask(ctx, instanceID, workflowName, e.Deadline)
		}
		return nil

	case eventNameWorkflowEventReceived:
		var e workflowEventReceived
		if err := json.Unmarshal(rec.Data(), &e); err != nil {
			return fmt.Errorf("unmarshal workflowEventReceived: %w", err)
		}
		if err := q.deleteWaitingEvent(ctx, instanceID, e.ReceivedEvent); err != nil {
			return err
		}
		workflowName := q.resolveWorkflowName(ctx, e.WorkflowName, instanceID)
		return q.upsertTask(ctx, instanceID, workflowName, time.Time{})

	case eventNameWorkflowCompleted, eventNameWorkflowFailed, eventNameWorkflowCancelled:
		if err := q.deleteAllWaitingEvents(ctx, instanceID); err != nil {
			return err
		}
		return q.deleteTask(ctx, instanceID)

	default:
		return nil
	}
}

// --- TaskQueue implementation ---

// Enqueue inserts a task into the ready_tasks table.
func (q *AsyncQueue) Enqueue(ctx context.Context, task QueuedTask) error {
	_, err := q.db.ExecContext(ctx, enqueueReadyTaskSQL,
		string(task.InstanceID), task.WorkflowName, queueTimeNS(task.RunAfter))
	if err != nil {
		return fmt.Errorf("async queue enqueue: %w", err)
	}
	return nil
}

// Poll preserves the ID-only API. Concurrent workers use PollClaim instead.
func (q *AsyncQueue) Poll(ctx context.Context) (*QueuedTask, error) {
	task, err := q.PollClaim(ctx)
	if err != nil || task == nil {
		return nil, err
	}
	q.legacyClaims.remember(*task)
	return &task.QueuedTask, nil
}

func (q *AsyncQueue) PollClaim(ctx context.Context) (*ClaimedTask, error) {
	return pollSQLiteClaim(ctx, q.db, q.nowFunc, q.leaseDuration, q.workerID)
}

func (q *AsyncQueue) claimLeaseDuration() time.Duration { return q.leaseDuration }

func (q *AsyncQueue) Complete(ctx context.Context, instanceID InstanceID) error {
	task, ok := q.legacyClaims.get(instanceID)
	if !ok {
		return nil
	}
	err := q.CompleteClaim(ctx, task)
	q.legacyClaims.forgetAcknowledged(task, err)
	return err
}

func (q *AsyncQueue) Fail(ctx context.Context, instanceID InstanceID) error {
	return q.Complete(ctx, instanceID)
}

func (q *AsyncQueue) ExtendLease(
	ctx context.Context,
	instanceID InstanceID,
	lease time.Duration,
) error {
	task, ok := q.legacyClaims.get(instanceID)
	if !ok {
		return nil
	}
	return q.ExtendClaim(ctx, task, lease)
}

func (q *AsyncQueue) CompleteClaim(ctx context.Context, task ClaimedTask) error {
	return acknowledgeSQLiteClaim(ctx, q.db, task, q.nowFunc)
}

func (q *AsyncQueue) FailClaim(ctx context.Context, task ClaimedTask) error {
	return q.CompleteClaim(ctx, task)
}

func (q *AsyncQueue) ExtendClaim(ctx context.Context, task ClaimedTask, lease time.Duration) error {
	return extendSQLiteClaim(ctx, q.db, task, q.nowFunc, lease)
}

func (q *AsyncQueue) ReleaseClaim(ctx context.Context, task ClaimedTask, runAfter time.Time) error {
	return releaseSQLiteClaim(ctx, q.db, task, q.nowFunc, runAfter)
}

// Len returns the total number of tasks in the queue (for testing).
func (q *AsyncQueue) Len() int {
	var count int
	_ = q.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM workflow_ready_tasks`).
		Scan(&count)
	return count
}

// --- Internal helpers ---

// upsertTask coalesces conservative wake notifications toward the earliest time.
// A lagging projection must not postpone a newer Enqueue wake. Run checks durable
// sleep/retry deadlines, so an early notification cannot execute delayed work.
// Active receipts and their pending successors use the same rules as Enqueue.
func (q *AsyncQueue) upsertTask(
	ctx context.Context,
	instanceID InstanceID,
	workflowName string,
	runAfter time.Time,
) error {
	runAfterNs := int64(0)
	if !runAfter.IsZero() {
		runAfterNs = runAfter.UnixNano()
	}

	_, err := q.db.ExecContext(
		ctx,
		enqueueReadyTaskSQL,
		string(instanceID),
		workflowName,
		runAfterNs,
	)
	if err != nil {
		return fmt.Errorf("async queue upsert task: %w", err)
	}
	return nil
}

// deleteTask removes a task from the ready_tasks table.
func (q *AsyncQueue) deleteTask(ctx context.Context, instanceID InstanceID) error {
	_, err := q.db.ExecContext(ctx, `
		DELETE FROM workflow_ready_tasks WHERE instance_id = ?
	`, string(instanceID))
	if err != nil {
		return fmt.Errorf("async queue delete task: %w", err)
	}
	return nil
}

// resolveWorkflowName returns the best available workflow name. It prefers the
// name embedded in the event payload (available since v2), falls back to the
// DB lookup, and only uses the instance ID as a last resort.
func (q *AsyncQueue) resolveWorkflowName(
	ctx context.Context,
	fromEvent string,
	instanceID InstanceID,
) string {
	if fromEvent != "" {
		return fromEvent
	}
	if name := q.getWorkflowName(ctx, instanceID); name != "" {
		return name
	}
	// Last resort — should not happen for events produced by v2+ code.
	return string(instanceID)
}

// getWorkflowName retrieves the workflow_name from the ready_tasks table for
// an existing task. Returns empty string if not found.
func (q *AsyncQueue) getWorkflowName(ctx context.Context, instanceID InstanceID) string {
	var name string
	_ = q.db.QueryRowContext(ctx, `
		SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?
	`, string(instanceID)).Scan(&name)
	return name
}

// insertWaitingEvent records a workflow waiting for an event.
func (q *AsyncQueue) insertWaitingEvent(
	ctx context.Context,
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

	_, err := q.db.ExecContext(ctx, `
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
func (q *AsyncQueue) isEventPublished(ctx context.Context, eventName string) (bool, error) {
	var count int
	if err := q.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_published_events WHERE event_name = ?
	`, eventName).Scan(&count); err != nil {
		return false, fmt.Errorf("async queue check published event: %w", err)
	}
	return count > 0, nil
}

// deleteWaitingEvent removes a specific waiter.
func (q *AsyncQueue) deleteWaitingEvent(
	ctx context.Context,
	instanceID InstanceID,
	eventName string,
) error {
	_, err := q.db.ExecContext(ctx, `
		DELETE FROM workflow_waiting_events
		WHERE instance_id = ? AND event_name = ?
	`, string(instanceID), eventName)
	if err != nil {
		return fmt.Errorf("async queue delete waiting event: %w", err)
	}
	return nil
}

// deleteAllWaitingEvents removes all waiters for an instance.
func (q *AsyncQueue) deleteAllWaitingEvents(ctx context.Context, instanceID InstanceID) error {
	_, err := q.db.ExecContext(ctx, `
		DELETE FROM workflow_waiting_events WHERE instance_id = ?
	`, string(instanceID))
	if err != nil {
		return fmt.Errorf("async queue delete all waiting events: %w", err)
	}
	return nil
}

// Compile-time interface checks.
var (
	_ ClaimedTaskQueue      = (*AsyncQueue)(nil)
	_ ClaimedTaskReleaser   = (*AsyncQueue)(nil)
	_ event.AsyncProjection = (*AsyncQueue)(nil)
)
