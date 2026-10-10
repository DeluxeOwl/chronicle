package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

// PostgresQueue schedules workflows atomically with PostgreSQL event writes.
// Separate hosts share receipt ownership and recover tasks after lease expiry.
// Receipts fence checkpoints, not external effects; activities must be idempotent.
type PostgresQueue struct {
	db                 *sql.DB
	workerID           string
	leaseDuration      time.Duration
	transactionTimeout time.Duration
	legacyClaims       legacyQueueClaims
}

// PostgresQueueOption configures a PostgresQueue.
type PostgresQueueOption func(*PostgresQueue)

// WithPostgresQueueLeaseDuration sets the positive initial lease duration.
// The default is 30 seconds. Lease timestamps always use the database clock.
func WithPostgresQueueLeaseDuration(d time.Duration) PostgresQueueOption {
	return func(q *PostgresQueue) { q.leaseDuration = d }
}

// WithPostgresQueueTransactionTimeout sets the positive storage transaction limit.
// The default is DefaultPostgresTransactionTimeout. Client execution, server
// statements, and idle transactions are bounded, but activity execution is not.
func WithPostgresQueueTransactionTimeout(d time.Duration) PostgresQueueOption {
	return func(q *PostgresQueue) { q.transactionTimeout = d }
}

// WithPostgresQueueWorkerID sets a diagnostic label, not an ownership token.
func WithPostgresQueueWorkerID(id string) PostgresQueueOption {
	return func(q *PostgresQueue) { q.workerID = id }
}

// NewPostgresQueue creates or upgrades the shared scheduling and waiting tables.
// Pass this queue to a transactional repository as its aggregate processor.
func NewPostgresQueue(
	ctx context.Context,
	db *sql.DB,
	opts ...PostgresQueueOption,
) (*PostgresQueue, error) {
	if db == nil {
		return nil, errors.New("postgres queue database cannot be nil")
	}
	q := &PostgresQueue{
		db:                 db,
		workerID:           "worker-" + string(newClaimReceipt()),
		leaseDuration:      defaultQueueLeaseDuration,
		transactionTimeout: DefaultPostgresTransactionTimeout,
		legacyClaims:       newLegacyQueueClaims(),
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("postgres queue option cannot be nil")
		}
		opt(q)
	}
	if err := validateLease(q.leaseDuration); err != nil {
		return nil, err
	}
	if q.transactionTimeout <= 0 {
		return nil, errors.New("postgres queue transaction timeout must be positive")
	}
	if err := ensurePostgresWorkflowSchema(ctx, db, q.transactionTimeout); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *PostgresQueue) Enqueue(ctx context.Context, task QueuedTask) error {
	return runPostgresWorkflowTx(
		ctx,
		q.db,
		q.transactionTimeout,
		func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, enqueuePostgresTaskSQL,
				string(task.InstanceID), task.WorkflowName, queueTimeNS(task.RunAfter))
			if err != nil {
				return fmt.Errorf("postgres queue enqueue: %w", err)
			}
			return nil
		},
	)
}

// Poll adapts receipt claims to the legacy ID-only API. Use PollClaim for workers.
func (q *PostgresQueue) Poll(ctx context.Context) (*QueuedTask, error) {
	task, err := q.PollClaim(ctx)
	if err != nil || task == nil {
		return nil, err
	}
	q.legacyClaims.remember(*task)
	return &task.QueuedTask, nil
}

func (q *PostgresQueue) PollClaim(ctx context.Context) (*ClaimedTask, error) {
	ctx, cancel := context.WithTimeout(ctx, q.transactionTimeout)
	defer cancel()
	receipt := newClaimReceipt()
	var id, name string
	var runAfterNS int64
	// SKIP LOCKED avoids competing workers waiting for the same task. Compute
	// the new expiry in UPDATE, after acquiring the candidate's row lock.
	err := q.db.QueryRowContext(ctx, `
		WITH candidate AS (
			SELECT instance_id FROM workflow_ready_tasks
			WHERE COALESCE(next_run_after_ns, run_after_ns) <= `+postgresClockNS+`
			AND (claimed_until_ns IS NULL OR claimed_until_ns <= `+postgresClockNS+`)
			ORDER BY COALESCE(next_run_after_ns, run_after_ns), instance_id
			LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE workflow_ready_tasks AS task SET
			run_after_ns = COALESCE(task.next_run_after_ns, task.run_after_ns),
			next_run_after_ns = NULL,
			claimed_by = $1, claimed_until_ns = `+postgresClockNS+` + $2::bigint,
			claim_receipt = $3
		FROM candidate WHERE task.instance_id = candidate.instance_id
		RETURNING task.instance_id, task.workflow_name, task.run_after_ns
	`, q.workerID, int64(q.leaseDuration), string(receipt)).Scan(&id, &name, &runAfterNS)
	if errors.Is(err, sql.ErrNoRows) {
		//nolint:nilnil // An empty queue is not an error.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres queue claim: %w", err)
	}
	task := &ClaimedTask{
		QueuedTask: QueuedTask{InstanceID: InstanceID(id), WorkflowName: name},
		Receipt:    receipt,
	}
	if runAfterNS != 0 {
		task.RunAfter = time.Unix(0, runAfterNS)
	}
	return task, nil
}

func (q *PostgresQueue) claimLeaseDuration() time.Duration { return q.leaseDuration }

func (q *PostgresQueue) Complete(ctx context.Context, id InstanceID) error {
	task, ok := q.legacyClaims.get(id)
	if !ok {
		return nil
	}
	err := q.CompleteClaim(ctx, task)
	q.legacyClaims.forgetAcknowledged(task, err)
	return err
}

func (q *PostgresQueue) Fail(ctx context.Context, id InstanceID) error {
	return q.Complete(ctx, id)
}

func (q *PostgresQueue) ExtendLease(ctx context.Context, id InstanceID, lease time.Duration) error {
	if err := validateLease(lease); err != nil {
		return err
	}
	task, ok := q.legacyClaims.get(id)
	if !ok {
		return nil
	}
	return q.ExtendClaim(ctx, task, lease)
}

func (q *PostgresQueue) CompleteClaim(ctx context.Context, task ClaimedTask) error {
	return q.withClaimTx(ctx, task, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM workflow_ready_tasks
			WHERE instance_id = $1 AND claim_receipt = $2
			AND claimed_until_ns > `+postgresClockNS+` AND next_run_after_ns IS NULL
		`, string(task.InstanceID), string(task.Receipt))
		if err != nil {
			return fmt.Errorf("postgres queue acknowledge delete: %w", err)
		}
		deleted, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("postgres queue acknowledge count: %w", err)
		}
		if deleted != 0 {
			return nil
		}
		result, err = tx.ExecContext(ctx, `
			UPDATE workflow_ready_tasks SET
				run_after_ns = next_run_after_ns, next_run_after_ns = NULL,
				claimed_by = NULL, claimed_until_ns = NULL, claim_receipt = NULL
			WHERE instance_id = $1 AND claim_receipt = $2
			AND claimed_until_ns > `+postgresClockNS+` AND next_run_after_ns IS NOT NULL
		`, string(task.InstanceID), string(task.Receipt))
		if err != nil {
			return fmt.Errorf("postgres queue acknowledge successor: %w", err)
		}
		return requireClaimChange(result)
	})
}

func (q *PostgresQueue) FailClaim(ctx context.Context, task ClaimedTask) error {
	return q.CompleteClaim(ctx, task)
}

func (q *PostgresQueue) ExtendClaim(
	ctx context.Context,
	task ClaimedTask,
	lease time.Duration,
) error {
	if err := validateLease(lease); err != nil {
		return err
	}
	return q.withClaimTx(ctx, task, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_ready_tasks
			SET claimed_until_ns = GREATEST(claimed_until_ns, `+postgresClockNS+` + $3::bigint)
			WHERE instance_id = $1 AND claim_receipt = $2
			AND claimed_until_ns > `+postgresClockNS+`
		`, string(task.InstanceID), string(task.Receipt), int64(lease))
		if err != nil {
			return fmt.Errorf("postgres queue extend claim: %w", err)
		}
		return requireClaimChange(result)
	})
}

func (q *PostgresQueue) ReleaseClaim(
	ctx context.Context,
	task ClaimedTask,
	runAfter time.Time,
) error {
	return q.withClaimTx(ctx, task, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE workflow_ready_tasks SET
				run_after_ns = COALESCE(next_run_after_ns, $3::bigint), next_run_after_ns = NULL,
				claimed_by = NULL, claimed_until_ns = NULL, claim_receipt = NULL
			WHERE instance_id = $1 AND claim_receipt = $2
			AND claimed_until_ns > `+postgresClockNS+`
		`, string(task.InstanceID), string(task.Receipt), queueTimeNS(runAfter))
		if err != nil {
			return fmt.Errorf("postgres queue release claim: %w", err)
		}
		return requireClaimChange(result)
	})
}

func (q *PostgresQueue) withClaimTx(
	ctx context.Context,
	task ClaimedTask,
	fn func(context.Context, *sql.Tx) error,
) error {
	if task.Receipt == "" {
		return ErrStaleClaim
	}
	return runPostgresWorkflowTx(
		ctx,
		q.db,
		q.transactionTimeout,
		func(ctx context.Context, tx *sql.Tx) error {
			if err := lockPostgresClaimTx(ctx, tx, task); err != nil {
				return err
			}
			return fn(ctx, tx)
		},
	)
}

// The clock query must follow FOR UPDATE. Time read before waiting for that
// lock could let an expired receipt mutate a task after another worker's claim.
func lockPostgresClaimTx(ctx context.Context, tx *sql.Tx, task ClaimedTask) error {
	if task.Receipt == "" {
		return ErrStaleClaim
	}
	var receipt sql.NullString
	var until sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT claim_receipt, claimed_until_ns FROM workflow_ready_tasks
		WHERE instance_id = $1 FOR UPDATE
	`, string(task.InstanceID)).Scan(&receipt, &until)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrStaleClaim
	}
	if err != nil {
		return fmt.Errorf("postgres queue lock claim: %w", err)
	}
	if !receipt.Valid || receipt.String != string(task.Receipt) || !until.Valid {
		return ErrStaleClaim
	}
	var nowNS int64
	if err := tx.QueryRowContext(ctx, "SELECT "+postgresClockNS).Scan(&nowNS); err != nil {
		return fmt.Errorf("postgres queue read clock: %w", err)
	}
	if until.Int64 <= nowNS {
		return ErrStaleClaim
	}
	return nil
}

// Process fences worker checkpoints inside the event append transaction, even
// when a replacement claimant has not changed the aggregate's event version.
func (q *PostgresQueue) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *WorkflowInstance,
	events aggregate.CommittedEvents[WorkflowEvent],
) error {
	if err := validatePostgresWaitTimeoutsTx(ctx, tx, events); err != nil {
		return err
	}
	if postgresEventsAffectWaits(events) {
		if err := lockPostgresWaitsTx(ctx, tx); err != nil {
			return err
		}
	}
	lease, claimed := ctx.Value(workerLeaseContextKey{}).(*workerLease)
	if claimed && lease != nil && lease.task != nil && lease.task.claim != nil {
		if lease.task.claim.InstanceID != root.id {
			return ErrStaleClaim
		}
		if err := lockPostgresClaimTx(ctx, tx, *lease.task.claim); err != nil {
			return err
		}
	}
	for evt := range events.All() {
		if err := q.processEvent(ctx, tx, root, evt); err != nil {
			return err
		}
	}
	return nil
}

func postgresEventsAffectWaits(events aggregate.CommittedEvents[WorkflowEvent]) bool {
	for evt := range events.All() {
		switch evt.(type) {
		case *workflowWaiting, *workflowEventReceived, *workflowCompleted, *workflowFailed, *workflowCancelled:
			return true
		case *stepStarted, *stepCompleted, *stepFailed, *workflowStarted, *workflowRetried:
			continue
		}
	}
	return false
}

func (q *PostgresQueue) processEvent(
	ctx context.Context,
	tx *sql.Tx,
	root *WorkflowInstance,
	evt WorkflowEvent,
) error {
	switch e := evt.(type) {
	case *workflowStarted:
		return q.scheduleTaskTx(ctx, tx, root, time.Time{})
	case *stepStarted, *stepFailed:
		return nil
	case *stepCompleted:
		var sleep sleepResult
		if (e.Kind == stepKindSleep || e.Kind == "") && json.Unmarshal(e.Result, &sleep) == nil && !sleep.WakeAt.IsZero() {
			return q.scheduleTaskTx(ctx, tx, root, sleep.WakeAt)
		}
		return nil
	case *workflowRetried:
		return q.scheduleTaskTx(ctx, tx, root, e.NextRunAfter)
	case *workflowWaiting:
		return q.scheduleWaitingEventTx(ctx, tx, root, e)
	case *workflowEventReceived:
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_waiting_events WHERE instance_id = $1 AND event_name = $2`, string(root.id), e.ReceivedEvent); err != nil {
			return fmt.Errorf("postgres queue delete waiter: %w", err)
		}
		return q.scheduleTaskTx(ctx, tx, root, time.Time{})
	case *workflowCompleted, *workflowFailed, *workflowCancelled:
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_waiting_events WHERE instance_id = $1`, string(root.id)); err != nil {
			return fmt.Errorf("postgres queue delete waiters: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_ready_tasks WHERE instance_id = $1`, string(root.id)); err != nil {
			return fmt.Errorf("postgres queue delete terminal task: %w", err)
		}
		return nil
	default:
		return nil
	}
}

func (q *PostgresQueue) scheduleTaskTx(
	ctx context.Context,
	tx *sql.Tx,
	root *WorkflowInstance,
	runAfter time.Time,
) error {
	if _, err := tx.ExecContext(ctx, schedulePostgresTaskSQL, string(root.id), root.workflowName, queueTimeNS(runAfter)); err != nil {
		return fmt.Errorf("postgres queue schedule task: %w", err)
	}
	return nil
}

func (q *PostgresQueue) scheduleWaitingEventTx(
	ctx context.Context,
	tx *sql.Tx,
	root *WorkflowInstance,
	evt *workflowWaiting,
) error {
	deadline := sql.NullInt64{Int64: queueTimeNS(evt.Deadline), Valid: !evt.Deadline.IsZero()}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_waiting_events (instance_id, event_name, workflow_name, step_index, deadline_ns)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (instance_id, event_name) DO UPDATE SET
			workflow_name = EXCLUDED.workflow_name, step_index = EXCLUDED.step_index,
			deadline_ns = EXCLUDED.deadline_ns
	`, string(root.id), evt.AwaitingEvent, root.workflowName, evt.StepIndex, deadline); err != nil {
		return fmt.Errorf("postgres queue register waiter: %w", err)
	}
	var published bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_published_events WHERE event_name = $1)`, evt.AwaitingEvent).Scan(&published); err != nil {
		return fmt.Errorf("postgres queue check publication: %w", err)
	}
	if published {
		return q.scheduleTaskTx(ctx, tx, root, time.Time{})
	}
	if !evt.Deadline.IsZero() {
		return q.scheduleTaskTx(ctx, tx, root, evt.Deadline)
	}
	return nil
}

// claimed_until_ns, not claimed_by, indicates a stored claim. This also keeps
// legacy claims with no receipt intact until they expire and are reclaimed.
const enqueuePostgresTaskSQL = `
	INSERT INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns)
	VALUES ($1, $2, $3)
	ON CONFLICT (instance_id) DO UPDATE SET
		workflow_name = EXCLUDED.workflow_name,
		run_after_ns = CASE WHEN workflow_ready_tasks.claimed_until_ns IS NULL
			THEN LEAST(workflow_ready_tasks.run_after_ns, EXCLUDED.run_after_ns)
			ELSE workflow_ready_tasks.run_after_ns END,
		next_run_after_ns = CASE WHEN workflow_ready_tasks.claimed_until_ns IS NOT NULL
			THEN LEAST(COALESCE(workflow_ready_tasks.next_run_after_ns, EXCLUDED.run_after_ns), EXCLUDED.run_after_ns)
			ELSE NULL END
`

const schedulePostgresTaskSQL = `
	INSERT INTO workflow_ready_tasks (instance_id, workflow_name, run_after_ns)
	VALUES ($1, $2, $3)
	ON CONFLICT (instance_id) DO UPDATE SET
		workflow_name = EXCLUDED.workflow_name,
		run_after_ns = CASE WHEN workflow_ready_tasks.claimed_until_ns IS NULL
			THEN EXCLUDED.run_after_ns ELSE workflow_ready_tasks.run_after_ns END,
		next_run_after_ns = CASE WHEN workflow_ready_tasks.claimed_until_ns IS NOT NULL
			THEN EXCLUDED.run_after_ns ELSE NULL END
`

// Numeric epoch conversion retains PostgreSQL's microsecond clock precision.
const postgresClockNS = "(EXTRACT(EPOCH FROM clock_timestamp()) * 1000000000)::bigint"

// Two-key advisory locks have a separate key space from the event log's
// single-key stream locks. Wait operations acquire this lock before task locks.
const (
	postgresWorkflowLockNamespace = int32(0x43485257) // CHRW
	postgresWorkflowSchemaLock    = int32(1)
	postgresWorkflowWaitLock      = int32(2)
)

func lockPostgresWaitsTx(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::integer, $2::integer)`, postgresWorkflowLockNamespace, postgresWorkflowWaitLock); err != nil {
		return fmt.Errorf("postgres workflow lock waits: %w", err)
	}
	// A snapshot taken by event append before waiting for the advisory lock
	// cannot see a concurrent publication under repeatable-read isolation.
	var isolation string
	if err := tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return fmt.Errorf("postgres workflow read isolation: %w", err)
	}
	if isolation != "read committed" {
		return errors.New("postgres workflow waits require read committed transaction isolation")
	}
	return nil
}

func ensurePostgresWorkflowSchema(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	return runPostgresWorkflowTx(ctx, db, timeout, func(ctx context.Context, tx *sql.Tx) error {
		// CREATE IF NOT EXISTS alone can race in PostgreSQL's system catalogs.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::integer, $2::integer)`, postgresWorkflowLockNamespace, postgresWorkflowSchemaLock); err != nil {
			return fmt.Errorf("postgres workflow lock schema: %w", err)
		}
		// DDL locks queue tables before publication tables. Exclude publishers
		// first: they would otherwise hold publication rows while awaiting tasks.
		if err := lockPostgresWaitsTx(ctx, tx); err != nil {
			return err
		}
		for _, statement := range []string{
			`CREATE TABLE IF NOT EXISTS workflow_ready_tasks (
			instance_id TEXT PRIMARY KEY, workflow_name TEXT NOT NULL,
			run_after_ns BIGINT NOT NULL DEFAULT 0, claimed_by TEXT,
			claimed_until_ns BIGINT, claim_receipt TEXT, next_run_after_ns BIGINT
		)`,
			`ALTER TABLE workflow_ready_tasks ADD COLUMN IF NOT EXISTS claim_receipt TEXT`,
			`ALTER TABLE workflow_ready_tasks ADD COLUMN IF NOT EXISTS next_run_after_ns BIGINT`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_ready_tasks_receipt
			ON workflow_ready_tasks (claim_receipt) WHERE claim_receipt IS NOT NULL`,
			`CREATE INDEX IF NOT EXISTS idx_workflow_ready_tasks_poll
			ON workflow_ready_tasks (COALESCE(next_run_after_ns, run_after_ns), instance_id)`,
			`CREATE TABLE IF NOT EXISTS workflow_waiting_events (
			instance_id TEXT NOT NULL, event_name TEXT NOT NULL,
			workflow_name TEXT NOT NULL, step_index INTEGER NOT NULL DEFAULT 0,
			deadline_ns BIGINT, PRIMARY KEY (instance_id, event_name)
		)`,
			`CREATE INDEX IF NOT EXISTS idx_workflow_waiting_events_name
			ON workflow_waiting_events (event_name, instance_id)`,
			`CREATE TABLE IF NOT EXISTS workflow_published_events (
			event_name TEXT PRIMARY KEY, payload TEXT NOT NULL,
			published_at_ns BIGINT NOT NULL DEFAULT 0
		)`,
			`ALTER TABLE workflow_published_events ADD COLUMN IF NOT EXISTS published_at_ns BIGINT NOT NULL DEFAULT 0`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("postgres workflow initialize schema: %w", err)
			}
		}
		return nil
	})
}

var (
	_ ClaimedTaskQueue = (*PostgresQueue)(
		nil,
	)
	_ ClaimedTaskReleaser = (*PostgresQueue)(
		nil,
	)
	_ aggregate.TransactionalAggregateProcessor[*sql.Tx, InstanceID, WorkflowEvent, *WorkflowInstance] = (*PostgresQueue)(
		nil,
	)
)
