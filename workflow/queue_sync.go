package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

// SyncQueue is a SQL-backed TaskQueue that atomically populates a ready_tasks
// table within the same transaction as event writes. This gives us Absurd/Temporal-level
// durability guarantees: if the process crashes after saving events, the tasks
// are already in the database and will be picked up by any worker on restart.
//
// SyncQueue implements both TaskQueue (for Poll/Complete/Fail/ExtendLease) and
// aggregate.TransactionalAggregateProcessor (for atomic task creation during event writes).
//
// Claiming uses row-level locking: Poll atomically selects and claims a ready task
// by setting claimed_by and claimed_until. If a worker crashes, the lease expires
// and the task becomes available for other workers.
type SyncQueue struct {
	db            *sql.DB
	nowFunc       func() time.Time
	workerID      string
	leaseDuration time.Duration
}

// SyncQueueOption configures a SyncQueue.
type SyncQueueOption func(*SyncQueue)

// WithSyncQueueNowFunc sets a custom clock for the queue.
// Defaults to time.Now.
func WithSyncQueueNowFunc(fn func() time.Time) SyncQueueOption {
	return func(q *SyncQueue) {
		q.nowFunc = fn
	}
}

// WithSyncQueueLeaseDuration sets the default lease duration when claiming a task.
// Defaults to 30 seconds. Workers should call ExtendLease (via Heartbeat) for
// long-running steps.
func WithSyncQueueLeaseDuration(d time.Duration) SyncQueueOption {
	return func(q *SyncQueue) {
		q.leaseDuration = d
	}
}

// WithSyncQueueWorkerID sets a custom worker ID for claiming tasks.
// Defaults to a timestamp-based ID. Each process should use a unique worker ID
// to prevent claim conflicts.
func WithSyncQueueWorkerID(id string) SyncQueueOption {
	return func(q *SyncQueue) {
		q.workerID = id
	}
}

// NewSyncQueue creates a SQL-backed task queue. It creates the ready_tasks table
// if it doesn't already exist.
//
// The SyncQueue must be passed as the TransactionalAggregateProcessor when creating
// a TransactionalRepository so that task creation is atomic with event writes.
// Use NewSqliteRunnerWithSyncQueue for convenient wiring.
func NewSyncQueue(db *sql.DB, opts ...SyncQueueOption) (*SyncQueue, error) {
	q := &SyncQueue{
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

	// Index for efficient polling: find unclaimed tasks ordered by run_after.
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_workflow_ready_tasks_poll
		ON workflow_ready_tasks (run_after_ns)
		WHERE claimed_by IS NULL
	`); err != nil {
		return nil, fmt.Errorf("create poll index: %w", err)
	}

	return q, nil
}

// --- TaskQueue implementation ---

// Enqueue inserts a task into the ready_tasks table. This is used by EmitEvent
// (which operates outside the event-save transaction) and as a fallback for
// backward compatibility. When used with a TransactionalRepository, the
// Process method handles atomic task creation, so this is often a harmless
// duplicate (INSERT OR REPLACE).
func (q *SyncQueue) Enqueue(_ context.Context, task QueuedTask) error {
	runAfterNs := int64(0)
	if !task.RunAfter.IsZero() {
		runAfterNs = task.RunAfter.UnixNano()
	}

	_, err := q.db.Exec(`
		INSERT OR REPLACE INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns, claimed_by, claimed_until_ns)
		VALUES (?, ?, ?, NULL, NULL)
	`, string(task.InstanceID), task.WorkflowName, runAfterNs)
	if err != nil {
		return fmt.Errorf("sync queue enqueue: %w", err)
	}

	return nil
}

// Poll finds the next ready task (run_after <= now, not claimed or lease expired),
// atomically claims it, and returns it. Returns nil, nil when no tasks are ready.
func (q *SyncQueue) Poll(ctx context.Context) (*QueuedTask, error) {
	now := q.nowFunc()
	nowNs := now.UnixNano()
	claimedUntilNs := now.Add(q.leaseDuration).UnixNano()

	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sync queue poll begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Find the next ready, unclaimed task.
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
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sync queue poll select: %w", err)
	}

	// Claim it with an optimistic lock: only succeed if it's still unclaimed.
	result, err := tx.ExecContext(ctx, `
		UPDATE workflow_ready_tasks
		SET claimed_by = ?, claimed_until_ns = ?
		WHERE instance_id = ?
		AND (claimed_by IS NULL OR claimed_until_ns <= ?)
	`, q.workerID, claimedUntilNs, instanceID, nowNs)
	if err != nil {
		return nil, fmt.Errorf("sync queue poll claim: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("sync queue poll rows affected: %w", err)
	}
	if affected == 0 {
		// Another worker claimed it between our SELECT and UPDATE — retry next poll.
		return nil, nil
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync queue poll commit: %w", err)
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

// Complete deletes the task claimed by this worker. If the task was already
// replaced by the processor (e.g., sleep created a delayed task), the
// claimed_by won't match and 0 rows are deleted — which is correct because
// the delayed task should survive.
func (q *SyncQueue) Complete(_ context.Context, instanceID InstanceID) error {
	_, err := q.db.Exec(`
		DELETE FROM workflow_ready_tasks
		WHERE instance_id = ? AND claimed_by = ?
	`, string(instanceID), q.workerID)
	if err != nil {
		return fmt.Errorf("sync queue complete: %w", err)
	}
	return nil
}

// Fail deletes the task claimed by this worker. Same semantics as Complete
// for the sync queue — the event log is the source of truth for failure state.
func (q *SyncQueue) Fail(_ context.Context, instanceID InstanceID) error {
	_, err := q.db.Exec(`
		DELETE FROM workflow_ready_tasks
		WHERE instance_id = ? AND claimed_by = ?
	`, string(instanceID), q.workerID)
	if err != nil {
		return fmt.Errorf("sync queue fail: %w", err)
	}
	return nil
}

// ExtendLease extends the claim timeout for a running task, signaling that
// the worker is still making progress on a long-running step.
func (q *SyncQueue) ExtendLease(_ context.Context, instanceID InstanceID, lease time.Duration) error {
	newDeadline := q.nowFunc().Add(lease).UnixNano()
	_, err := q.db.Exec(`
		UPDATE workflow_ready_tasks
		SET claimed_until_ns = ?
		WHERE instance_id = ? AND claimed_by = ?
	`, newDeadline, string(instanceID), q.workerID)
	if err != nil {
		return fmt.Errorf("sync queue extend lease: %w", err)
	}
	return nil
}

// Len returns the total number of tasks in the queue (for testing).
func (q *SyncQueue) Len() int {
	var count int
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM workflow_ready_tasks`).Scan(&count)
	return count
}

// --- TransactionalAggregateProcessor implementation ---
//
// Process is called inside the same database transaction as the event append.
// It inspects the committed events and atomically inserts/deletes tasks in
// the ready_tasks table.
//
// Event → Task mapping:
//   - workflowStarted      → UPSERT immediate task
//   - stepCompleted (sleep) → UPSERT delayed task (run_after = wakeAt)
//   - workflowRetried       → UPSERT delayed task (run_after = nextRunAfter)
//   - workflowWaiting       → UPSERT delayed task if deadline set, else no task
//   - workflowEventReceived → UPSERT immediate task (continue execution)
//   - workflowCompleted     → DELETE task
//   - workflowFailed        → DELETE task
//   - stepFailed            → no action (workflow function handles it)
func (q *SyncQueue) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *WorkflowInstance,
	events aggregate.CommittedEvents[WorkflowEvent],
) error {
	for evt := range events.All() {
		if err := q.processEvent(ctx, tx, root, evt); err != nil {
			return err
		}
	}
	return nil
}

func (q *SyncQueue) processEvent(
	ctx context.Context,
	tx *sql.Tx,
	root *WorkflowInstance,
	evt WorkflowEvent,
) error {
	switch e := evt.(type) {
	case *workflowStarted:
		return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, q.nowFunc())

	case *stepCompleted:
		// Check if this is a sleep step by trying to parse the result as sleepResult.
		var sr sleepResult
		if err := json.Unmarshal(e.Result, &sr); err == nil && !sr.WakeAt.IsZero() {
			return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, sr.WakeAt)
		}
		// Regular step — no task action needed.
		return nil

	case *workflowRetried:
		return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, e.NextRunAfter)

	case *workflowWaiting:
		if !e.Deadline.IsZero() {
			// Timeout configured — schedule a wake-up at the deadline.
			return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, e.Deadline)
		}
		// No deadline — the workflow is parked until EmitEvent creates a task.
		return nil

	case *workflowEventReceived:
		// Event arrived — schedule immediate re-execution.
		return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, q.nowFunc())

	case *workflowCompleted:
		return q.deleteTaskTx(ctx, tx, root.id)

	case *workflowFailed:
		return q.deleteTaskTx(ctx, tx, root.id)

	case *stepFailed:
		// Step failure is handled by the workflow function (retry or fail).
		return nil

	default:
		// Unknown event — ignore silently.
		return nil
	}
}

// upsertTaskTx inserts or replaces a task within an existing transaction.
// INSERT OR REPLACE removes the old row (including any claim) and inserts
// a fresh one. This is intentional: when a workflow sleeps or retries, the
// old claimed task is replaced with a new unclaimed delayed task.
func (q *SyncQueue) upsertTaskTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID InstanceID,
	workflowName string,
	runAfter time.Time,
) error {
	runAfterNs := int64(0)
	if !runAfter.IsZero() {
		runAfterNs = runAfter.UnixNano()
	}

	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns, claimed_by, claimed_until_ns)
		VALUES (?, ?, ?, NULL, NULL)
	`, string(instanceID), workflowName, runAfterNs)
	if err != nil {
		return fmt.Errorf("sync queue upsert task: %w", err)
	}
	return nil
}

// deleteTaskTx removes a task within an existing transaction.
func (q *SyncQueue) deleteTaskTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID InstanceID,
) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM workflow_ready_tasks WHERE instance_id = ?
	`, string(instanceID))
	if err != nil {
		return fmt.Errorf("sync queue delete task: %w", err)
	}
	return nil
}

// Compile-time interface checks.
var (
	_ TaskQueue = (*SyncQueue)(nil)
	_ aggregate.TransactionalAggregateProcessor[*sql.Tx, InstanceID, WorkflowEvent, *WorkflowInstance] = (*SyncQueue)(nil)
)
