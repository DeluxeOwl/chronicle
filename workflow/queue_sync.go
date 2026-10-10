package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

// SyncQueue stores tasks in SQLite within the event-save transaction. Tasks
// survive process restarts. A single UPDATE claims each task with a unique
// receipt; expired claims can be recovered by another execution. Queue leases
// do not guarantee exactly-once external workflow effects.
type SyncQueue struct {
	db            *sql.DB
	nowFunc       func() time.Time
	workerID      string
	leaseDuration time.Duration
	legacyClaims  legacyQueueClaims
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
// Defaults to a timestamp-based ID. Receipts, rather than worker IDs, enforce
// ownership; the worker ID is diagnostic.
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
func NewSyncQueue(ctx context.Context, db *sql.DB, opts ...SyncQueueOption) (*SyncQueue, error) {
	q := &SyncQueue{
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

	// Tables for persistent event waiting (used by persistentWaitStore +
	// transactional inserts in Process).
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

// --- TaskQueue implementation ---

// Enqueue inserts a task into the ready_tasks table. This is used by PublishEvent
// (which operates outside the event-save transaction) and as a fallback for
// backward compatibility. When used with a TransactionalRepository, the
// Process method handles atomic task creation, so this is often a redundant
// write for the same task.
//
// Uses MIN(run_after_ns) on conflict to avoid overwriting a better (earlier)
// run_after created atomically by Process. For example, when Process detects
// a pre-published event and creates an immediate task, a subsequent Enqueue
// from WaitForEvent with a deadline must not overwrite it.
func (q *SyncQueue) Enqueue(ctx context.Context, task QueuedTask) error {
	_, err := q.db.ExecContext(ctx, enqueueReadyTaskSQL,
		string(task.InstanceID), task.WorkflowName, queueTimeNS(task.RunAfter))
	if err != nil {
		return fmt.Errorf("sync queue enqueue: %w", err)
	}
	return nil
}

// Poll preserves the ID-only TaskQueue API. Concurrent workers should use
// PollClaim and receipt-aware acknowledgments instead.
func (q *SyncQueue) Poll(ctx context.Context) (*QueuedTask, error) {
	task, err := q.PollClaim(ctx)
	if err != nil || task == nil {
		return nil, err
	}
	q.legacyClaims.remember(*task)
	return &task.QueuedTask, nil
}

func (q *SyncQueue) PollClaim(ctx context.Context) (*ClaimedTask, error) {
	return pollSQLiteClaim(ctx, q.db, q.nowFunc, q.leaseDuration, q.workerID)
}

func (q *SyncQueue) claimLeaseDuration() time.Duration { return q.leaseDuration }

func (q *SyncQueue) Complete(ctx context.Context, instanceID InstanceID) error {
	task, ok := q.legacyClaims.get(instanceID)
	if !ok {
		return nil
	}
	err := q.CompleteClaim(ctx, task)
	q.legacyClaims.forgetAcknowledged(task, err)
	return err
}

func (q *SyncQueue) Fail(ctx context.Context, instanceID InstanceID) error {
	return q.Complete(ctx, instanceID)
}

func (q *SyncQueue) ExtendLease(
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

func (q *SyncQueue) CompleteClaim(ctx context.Context, task ClaimedTask) error {
	return acknowledgeSQLiteClaim(ctx, q.db, task, q.nowFunc)
}

func (q *SyncQueue) FailClaim(ctx context.Context, task ClaimedTask) error {
	return q.CompleteClaim(ctx, task)
}

func (q *SyncQueue) ExtendClaim(ctx context.Context, task ClaimedTask, lease time.Duration) error {
	return extendSQLiteClaim(ctx, q.db, task, q.nowFunc, lease)
}

func (q *SyncQueue) ReleaseClaim(ctx context.Context, task ClaimedTask, runAfter time.Time) error {
	return releaseSQLiteClaim(ctx, q.db, task, q.nowFunc, runAfter)
}

// Len returns the total number of tasks in the queue (for testing).
func (q *SyncQueue) Len() int {
	var count int
	_ = q.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM workflow_ready_tasks`).
		Scan(&count)
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
	if err := validateSQLiteWaitTimeoutsTx(ctx, tx, events); err != nil {
		return err
	}
	if err := q.validateExecutionClaimTx(ctx, tx, root.id); err != nil {
		return err
	}
	for evt := range events.All() {
		if err := q.processEvent(ctx, tx, root, evt); err != nil {
			return err
		}
	}
	return nil
}

// Event version checks cannot fence an expired worker when the new claimant
// has not written an event yet. Validate ownership in this same transaction;
// a rejection rolls back both the event append and queue projection.
func (q *SyncQueue) validateExecutionClaimTx(ctx context.Context, tx *sql.Tx, id InstanceID) error {
	lease, ok := ctx.Value(workerLeaseContextKey{}).(*workerLease)
	if !ok || lease.task.claim == nil {
		return nil
	} // Direct commands are not worker claims.
	claim := lease.task.claim
	if claim.InstanceID != id || claim.Receipt == "" {
		return ErrStaleClaim
	}
	var owned int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM workflow_ready_tasks WHERE instance_id=? AND claim_receipt=? AND claimed_until_ns>?`, string(id), string(claim.Receipt), q.nowFunc().UnixNano()).
		Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleClaim
	}
	if err != nil {
		return fmt.Errorf("validate execution claim: %w", err)
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
		return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, time.Time{})

	case *stepStarted:
		return nil

	case *stepCompleted:
		// Check if this is a sleep step by trying to parse the result as sleepResult.
		var sr sleepResult
		if (e.Kind == "sleep" || e.Kind == "") && json.Unmarshal(e.Result, &sr) == nil && !sr.WakeAt.IsZero() {
			return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, sr.WakeAt)
		}
		// Regular step — no task action needed.
		return nil

	case *workflowRetried:
		return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, e.NextRunAfter)

	case *workflowWaiting:
		// Record the waiter in the persistent waiting_events table.
		if err := q.insertWaitingEventTx(ctx, tx, root.id, root.workflowName, e.AwaitingEvent, e.StepIndex, e.Deadline); err != nil {
			return err
		}
		// Check if the event was already published (pre-publish case).
		// If so, create an immediate task so the worker picks it up.
		published, err := q.isEventPublishedTx(ctx, tx, e.AwaitingEvent)
		if err != nil {
			return err
		}
		if published {
			return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, time.Time{})
		}
		if !e.Deadline.IsZero() {
			// Timeout configured — schedule a wake-up at the deadline.
			return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, e.Deadline)
		}
		// No deadline and no pre-published event — the workflow is parked
		// until PublishEvent creates a task.
		return nil

	case *workflowEventReceived:
		// Clean up the waiter from the persistent table.
		if err := q.deleteWaitingEventTx(ctx, tx, root.id, e.ReceivedEvent); err != nil {
			return err
		}
		// Event arrived — schedule immediate re-execution.
		return q.upsertTaskTx(ctx, tx, root.id, root.workflowName, time.Time{})

	case *workflowCompleted, *workflowFailed, *workflowCancelled:
		if err := q.deleteAllWaitingEventsTx(ctx, tx, root.id); err != nil {
			return err
		}
		return q.deleteTaskTx(ctx, tx, root.id)

	case *stepFailed:
		// Step failure is handled by the workflow function (retry or fail).
		return nil

	default:
		// Unknown event — ignore silently.
		return nil
	}
}

// upsertTaskTx schedules the authoritative next execution. An active claim
// remains owned until acknowledgment or expiry; its follow-up is not deleted
// by acknowledgment of the old execution.
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

	_, err := tx.ExecContext(
		ctx,
		scheduleReadyTaskSQL,
		string(instanceID),
		workflowName,
		runAfterNs,
	)
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

// --- Shared SQLite claim storage ---

// Both scheduling paths preserve active ownership. Enqueue coalesces duplicate
// notifications toward the earliest time; projection scheduling sets the next
// execution time from the durable workflow event.
const enqueueReadyTaskSQL = `
	INSERT INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns)
	VALUES (?, ?, ?)
	ON CONFLICT(instance_id) DO UPDATE SET
		workflow_name = excluded.workflow_name,
		run_after_ns = CASE WHEN workflow_ready_tasks.claimed_by IS NULL
			THEN MIN(workflow_ready_tasks.run_after_ns, excluded.run_after_ns)
			ELSE workflow_ready_tasks.run_after_ns END,
		next_run_after_ns = CASE WHEN workflow_ready_tasks.claimed_by IS NOT NULL
			THEN MIN(COALESCE(workflow_ready_tasks.next_run_after_ns, excluded.run_after_ns), excluded.run_after_ns)
			ELSE NULL END
`

const scheduleReadyTaskSQL = `
	INSERT INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns)
	VALUES (?, ?, ?)
	ON CONFLICT(instance_id) DO UPDATE SET
		workflow_name = excluded.workflow_name,
		run_after_ns = CASE WHEN workflow_ready_tasks.claimed_by IS NULL
			THEN excluded.run_after_ns ELSE workflow_ready_tasks.run_after_ns END,
		next_run_after_ns = CASE WHEN workflow_ready_tasks.claimed_by IS NOT NULL
			THEN excluded.run_after_ns ELSE NULL END
`

// schemaMu serializes additive upgrades made by queue constructors in this
// process. ALTER is also rechecked after failure for concurrent processes.
var queueSchemaMu sync.Mutex

func ensureReadyTasksSchema(ctx context.Context, db *sql.DB) error {
	queueSchemaMu.Lock()
	defer queueSchemaMu.Unlock()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS workflow_ready_tasks (
			instance_id TEXT PRIMARY KEY,
			workflow_name TEXT NOT NULL,
			run_after_ns INTEGER NOT NULL DEFAULT 0,
			claimed_by TEXT,
			claimed_until_ns INTEGER,
			claim_receipt TEXT,
			next_run_after_ns INTEGER
		)
	`); err != nil {
		return fmt.Errorf("create ready_tasks table: %w", err)
	}
	for _, column := range []struct{ name, definition string }{
		{name: "claim_receipt", definition: "TEXT"},
		{name: "next_run_after_ns", definition: "INTEGER"},
	} {
		if err := ensureSQLiteColumn(ctx, db, "workflow_ready_tasks", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_workflow_ready_tasks_poll
		ON workflow_ready_tasks (run_after_ns)
		WHERE claimed_by IS NULL
	`); err != nil {
		return fmt.Errorf("create poll index: %w", err)
	}
	return nil
}

func ensurePublishedEventsSchema(ctx context.Context, db *sql.DB) error {
	queueSchemaMu.Lock()
	defer queueSchemaMu.Unlock()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS workflow_published_events (
			event_name TEXT PRIMARY KEY,
			payload TEXT NOT NULL,
			published_at_ns INTEGER NOT NULL DEFAULT 0
		)
	`); err != nil {
		return fmt.Errorf("create published_events table: %w", err)
	}
	return ensureSQLiteColumn(
		ctx,
		db,
		"workflow_published_events",
		"published_at_ns",
		"INTEGER NOT NULL DEFAULT 0",
	)
}

// Identifiers are internal constants, never user input.
func ensureSQLiteColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	present, err := sqliteColumnExists(ctx, db, table, column)
	if err != nil || present {
		return err
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition); err != nil {
		present, checkErr := sqliteColumnExists(ctx, db, table, column)
		if checkErr == nil && present {
			return nil // A concurrent constructor already added the column.
		}
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func sqliteColumnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, fmt.Errorf("inspect %s schema: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var index, notNull, primaryKey int
		var name, dataType string
		var defaultValue sql.NullString
		if err := rows.Scan(&index, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("read %s schema: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read %s schema: %w", table, err)
	}
	return false, nil
}

func queueTimeNS(at time.Time) int64 {
	if at.IsZero() {
		return 0
	}
	return at.UnixNano()
}

// Acquire the write lock before reading the clock. Sampling time before a
// connection or lock wait can renew an expired receipt or return an expired
// new claim. The no-op UPDATE enters SQLite's write transaction without
// modifying task rows or invoking row triggers.
func withSQLiteWriteTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow write transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // A committed transaction cannot roll back.
	if _, err := tx.ExecContext(ctx, "UPDATE workflow_ready_tasks SET claimed_by=claimed_by WHERE 0"); err != nil {
		return fmt.Errorf("acquire workflow write lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow write transaction: %w", err)
	}
	return nil
}

func pollSQLiteClaim(
	ctx context.Context,
	db *sql.DB,
	nowFunc func() time.Time,
	lease time.Duration,
	workerID string,
) (*ClaimedTask, error) {
	var task *ClaimedTask
	err := withSQLiteWriteTx(ctx, db, func(tx *sql.Tx) error {
		now := nowFunc()
		receipt := newClaimReceipt()
		var instanceID, workflowName string
		var runAfterNS int64
		err := tx.QueryRowContext(ctx, `
			UPDATE workflow_ready_tasks SET
				run_after_ns=COALESCE(next_run_after_ns,run_after_ns),next_run_after_ns=NULL,
				claimed_by=?,claimed_until_ns=?,claim_receipt=?
			WHERE instance_id=(
				SELECT instance_id FROM workflow_ready_tasks
				WHERE COALESCE(next_run_after_ns,run_after_ns)<=?
				AND (claimed_by IS NULL OR claimed_until_ns IS NULL OR claimed_until_ns<=?)
				ORDER BY COALESCE(next_run_after_ns,run_after_ns),instance_id LIMIT 1
			)
			RETURNING instance_id,workflow_name,run_after_ns
		`, workerID, now.Add(lease).UnixNano(), string(receipt), now.UnixNano(), now.UnixNano()).Scan(&instanceID, &workflowName, &runAfterNS)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("claim ready task: %w", err)
		}
		task = &ClaimedTask{
			QueuedTask: QueuedTask{InstanceID: InstanceID(instanceID), WorkflowName: workflowName},
			Receipt:    receipt,
		}
		if runAfterNS != 0 {
			task.RunAfter = time.Unix(0, runAfterNS)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}

func acknowledgeSQLiteClaim(
	ctx context.Context,
	db *sql.DB,
	task ClaimedTask,
	nowFunc func() time.Time,
) error {
	if task.Receipt == "" {
		return ErrStaleClaim
	}
	return withSQLiteWriteTx(ctx, db, func(tx *sql.Tx) error {
		now := nowFunc()
		// Delete only the acknowledged execution, never a pending successor.
		result, err := tx.ExecContext(ctx, `
			DELETE FROM workflow_ready_tasks
			WHERE instance_id=? AND claim_receipt=? AND claimed_until_ns>?
			AND next_run_after_ns IS NULL
		`, string(task.InstanceID), string(task.Receipt), now.UnixNano())
		if err != nil {
			return fmt.Errorf("acknowledge task delete: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("acknowledge task count: %w", err)
		}
		if deleted != 0 {
			return nil
		}
		result, err = tx.ExecContext(ctx, `
			UPDATE workflow_ready_tasks SET
				run_after_ns=next_run_after_ns,next_run_after_ns=NULL,
				claimed_by=NULL,claimed_until_ns=NULL,claim_receipt=NULL
			WHERE instance_id=? AND claim_receipt=? AND claimed_until_ns>?
			AND next_run_after_ns IS NOT NULL
		`, string(task.InstanceID), string(task.Receipt), nowFunc().UnixNano())
		if err != nil {
			return fmt.Errorf("acknowledge task successor: %w", err)
		}
		return requireClaimChange(result)
	})
}

func extendSQLiteClaim(
	ctx context.Context,
	db *sql.DB,
	task ClaimedTask,
	nowFunc func() time.Time,
	lease time.Duration,
) error {
	if err := validateLease(lease); err != nil {
		return err
	}
	if task.Receipt == "" {
		return ErrStaleClaim
	}
	return withSQLiteWriteTx(ctx, db, func(tx *sql.Tx) error {
		now := nowFunc()
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_ready_tasks
			SET claimed_until_ns=MAX(claimed_until_ns,?)
			WHERE instance_id=? AND claim_receipt=? AND claimed_until_ns>?
		`, now.Add(lease).UnixNano(), string(task.InstanceID), string(task.Receipt), now.UnixNano())
		if err != nil {
			return fmt.Errorf("extend task claim: %w", err)
		}
		return requireClaimChange(result)
	})
}

func releaseSQLiteClaim(
	ctx context.Context,
	db *sql.DB,
	task ClaimedTask,
	nowFunc func() time.Time,
	runAfter time.Time,
) error {
	if task.Receipt == "" {
		return ErrStaleClaim
	}
	return withSQLiteWriteTx(ctx, db, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_ready_tasks SET
				run_after_ns=COALESCE(next_run_after_ns,?),next_run_after_ns=NULL,
				claimed_by=NULL,claimed_until_ns=NULL,claim_receipt=NULL
			WHERE instance_id=? AND claim_receipt=? AND claimed_until_ns>?
		`, queueTimeNS(runAfter), string(task.InstanceID), string(task.Receipt), nowFunc().UnixNano())
		if err != nil {
			return fmt.Errorf("release task claim: %w", err)
		}
		return requireClaimChange(result)
	})
}

func requireClaimChange(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("claim rows affected: %w", err)
	}
	if count == 0 {
		return ErrStaleClaim
	}
	return nil
}

// Legacy ID-only calls can track one receipt per instance, but cannot disambiguate
// concurrent callers with the same ID. RunWorkers never uses this compatibility path.
type legacyQueueClaims struct {
	mu     sync.Mutex
	claims map[InstanceID]ClaimedTask
}

func newLegacyQueueClaims() legacyQueueClaims {
	return legacyQueueClaims{mu: sync.Mutex{}, claims: make(map[InstanceID]ClaimedTask)}
}

func (c *legacyQueueClaims) remember(task ClaimedTask) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claims[task.InstanceID] = task
}

func (c *legacyQueueClaims) get(id InstanceID) (ClaimedTask, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	task, ok := c.claims[id]
	return task, ok
}

func (c *legacyQueueClaims) forgetAcknowledged(task ClaimedTask, err error) {
	if err != nil && !errors.Is(err, ErrStaleClaim) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.claims[task.InstanceID]; ok && current.Receipt == task.Receipt {
		delete(c.claims, task.InstanceID)
	}
}

// --- Waiting events table helpers (transactional) ---

// insertWaitingEventTx records a workflow waiting for an event within the
// event-save transaction.
func (q *SyncQueue) insertWaitingEventTx(
	ctx context.Context,
	tx *sql.Tx,
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

	_, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO workflow_waiting_events
			(instance_id, event_name, workflow_name, step_index, deadline_ns)
		VALUES (?, ?, ?, ?, ?)
	`, string(instanceID), eventName, workflowName, stepIndex, deadlineNs)
	if err != nil {
		return fmt.Errorf("sync queue insert waiting event: %w", err)
	}
	return nil
}

// isEventPublishedTx checks if an event has already been published by querying
// the workflow_published_events table within the current transaction.
func (q *SyncQueue) isEventPublishedTx(
	ctx context.Context,
	tx *sql.Tx,
	eventName string,
) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workflow_published_events WHERE event_name = ?
	`, eventName).Scan(&count); err != nil {
		return false, fmt.Errorf("sync queue check published event: %w", err)
	}
	return count > 0, nil
}

// deleteWaitingEventTx removes a specific waiter within the event-save transaction.
func (q *SyncQueue) deleteWaitingEventTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID InstanceID,
	eventName string,
) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM workflow_waiting_events
		WHERE instance_id = ? AND event_name = ?
	`, string(instanceID), eventName)
	if err != nil {
		return fmt.Errorf("sync queue delete waiting event: %w", err)
	}
	return nil
}

// deleteAllWaitingEventsTx removes all waiters for an instance within
// the event-save transaction (used on workflow completion/failure).
func (q *SyncQueue) deleteAllWaitingEventsTx(
	ctx context.Context,
	tx *sql.Tx,
	instanceID InstanceID,
) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM workflow_waiting_events WHERE instance_id = ?
	`, string(instanceID))
	if err != nil {
		return fmt.Errorf("sync queue delete all waiting events: %w", err)
	}
	return nil
}

// Compile-time interface checks.
var (
	_ ClaimedTaskQueue = (*SyncQueue)(
		nil,
	)
	_ ClaimedTaskReleaser = (*SyncQueue)(
		nil,
	)
	_ aggregate.TransactionalAggregateProcessor[*sql.Tx, InstanceID, WorkflowEvent, *WorkflowInstance] = (*SyncQueue)(
		nil,
	)
)
