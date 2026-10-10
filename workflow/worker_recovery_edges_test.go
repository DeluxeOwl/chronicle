package workflow_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func freshWorkerReviewLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

type freshLegacyCleanupQueue struct {
	workflow.TaskQueue // Deliberately exposes only the supported legacy API.
	failNextEnqueue    atomic.Bool
	cleanupFailed      chan struct{}
}

func (q *freshLegacyCleanupQueue) Enqueue(ctx context.Context, task workflow.QueuedTask) error {
	if q.failNextEnqueue.CompareAndSwap(true, false) {
		close(q.cleanupFailed)
		return errors.New("one transient enqueue outage")
	}
	return q.TaskQueue.Enqueue(ctx, task)
}

func TestWorkerRecoveryEdges_LegacyCleanupFailureMustNotLoseWork(t *testing.T) {
	memory := workflow.NewMemoryQueue()
	queue := &freshLegacyCleanupQueue{
		TaskQueue:       memory,
		failNextEnqueue: atomic.Bool{},
		cleanupFailed:   make(chan struct{}),
	}
	runner, err := workflow.NewRunner(
		eventlog.NewMemory(),
		freshWorkerReviewLogger(),
		workflow.WithTaskQueue(queue),
	)
	require.NoError(t, err)
	var attempts atomic.Int32
	wf := workflow.New(
		runner,
		"legacy-release-recovery",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(context.Context) error {
				if attempts.Add(1) == 1 {
					queue.failNextEnqueue.Store(true)
					return errors.Join(
						workflow.ErrWorkflowInfrastructure,
						errors.New("transient execution failure"),
					)
				}
				return nil
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err = runner.RunWorker(
		ctx,
		workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-queue.cleanupFailed:
	default:
		t.Fatal("cleanup fault was not exercised")
	}
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	if attempts.Load() != 2 || info.Status != workflow.StatusCompleted {
		t.Fatalf(
			"one transient cleanup failure lost the task: attempts=%d, status=%s, queued=%d; no outage remains",
			attempts.Load(),
			info.Status,
			memory.Len(),
		)
	}
}

type legacyRetentionQueue struct {
	workflow.TaskQueue
	unavailable atomic.Bool
	retrying    chan struct{}
	err         error
}

func (q *legacyRetentionQueue) Enqueue(ctx context.Context, task workflow.QueuedTask) error {
	if q.unavailable.Load() {
		select {
		case q.retrying <- struct{}{}:
		default:
		}
		return q.err
	}
	return q.TaskQueue.Enqueue(ctx, task)
}

func TestWorkerRecoveryEdges_PermanentLegacyCleanupFailureSurvivesPoolCancellation(t *testing.T) {
	memory := workflow.NewMemoryQueue()
	outage := errors.New("legacy queue unavailable")
	queue := &legacyRetentionQueue{
		TaskQueue: memory, unavailable: atomic.Bool{}, retrying: make(chan struct{}, 1), err: outage,
	}
	runner, err := workflow.NewRunner(
		eventlog.NewMemory(),
		freshWorkerReviewLogger(),
		workflow.WithTaskQueue(queue),
	)
	require.NoError(t, err)
	var attempts atomic.Int32
	wf := workflow.New(
		runner,
		"unretained-legacy-task",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(context.Context) error {
				attempts.Add(1)
				queue.unavailable.Store(true)
				return workflow.ErrWorkflowInfrastructure
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	bound, stopBound := context.WithTimeout(t.Context(), 7*time.Second)
	defer stopBound()
	ctx, cancel := context.WithCancel(bound)
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorkers(ctx, 2, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	defer cancel()
	select {
	case <-queue.retrying:
	case <-bound.Done():
		t.Fatal("legacy cleanup did not begin")
	}
	cancel()
	select {
	case err = <-done:
	case <-bound.Done():
		t.Fatal("legacy cleanup exceeded its five-second bound")
	}
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the cleanup timeout must be observable")
	require.ErrorIs(
		t,
		err,
		outage,
		"an idle worker's cancellation must not hide the retention error",
	)
	require.ErrorContains(
		t,
		err,
		string(id),
		"the caller needs the instance ID to repair scheduling",
	)
	require.Equal(t, int32(1), attempts.Load(), "failed requeues must not rerun the callback")
	require.Zero(t, memory.Len(), "a destructive queue has no durable notification to recover")
}

type freshClaimCleanupQueue struct {
	*workflow.MemoryQueue
	failedRenewal atomic.Bool
	failedCleanup atomic.Bool
	cleanupFailed chan struct{}
}

func (q *freshClaimCleanupQueue) ExtendClaim(
	ctx context.Context,
	task workflow.ClaimedTask,
	lease time.Duration,
) error {
	if q.failedRenewal.CompareAndSwap(false, true) {
		return errors.New("one transient renewal outage")
	}
	return q.MemoryQueue.ExtendClaim(ctx, task, lease)
}

func (q *freshClaimCleanupQueue) ReleaseClaim(
	ctx context.Context,
	task workflow.ClaimedTask,
	runAfter time.Time,
) error {
	if q.failedCleanup.CompareAndSwap(false, true) {
		close(q.cleanupFailed)
		return errors.New("one transient release outage")
	}
	return q.MemoryQueue.ReleaseClaim(ctx, task, runAfter)
}

func TestWorkerRecoveryEdges_ClaimRecoverySurvivesRenewalAndCleanupFailure(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	queue := &freshClaimCleanupQueue{
		MemoryQueue:   workflow.NewMemoryQueue(workflow.WithMemoryQueueNowFunc(clock.Now)),
		failedRenewal: atomic.Bool{},
		failedCleanup: atomic.Bool{},
		cleanupFailed: make(chan struct{}),
	}
	runner, err := workflow.NewRunner(
		eventlog.NewMemory(),
		freshWorkerReviewLogger(),
		workflow.WithTaskQueue(queue),
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	var attempts atomic.Int32
	interrupted := make(chan struct{})
	wf := workflow.New(
		runner,
		"claimed-release-recovery",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				if attempts.Add(1) == 1 {
					<-ctx.Done()
					close(interrupted)
					return ctx.Err()
				}
				return nil
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: time.Millisecond})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker failed to join")
		}
	})
	select {
	case <-queue.cleanupFailed:
	case <-ctx.Done():
		t.Fatal("renewal/release faults were not exercised")
	}
	select {
	case <-interrupted:
	default:
		t.Fatal("renewal failure did not interrupt the callback")
	}
	clock.Advance(time.Minute)
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(t.Context(), id)
		return err == nil
	}, 500*time.Millisecond, time.Millisecond)
	require.Equal(t, int32(2), attempts.Load())
}

type freshObservedClaimQueue struct {
	*workflow.MemoryQueue
	claimed chan workflow.ClaimedTask
}

func (q *freshObservedClaimQueue) PollClaim(ctx context.Context) (*workflow.ClaimedTask, error) {
	claim, err := q.MemoryQueue.PollClaim(ctx)
	if err == nil && claim != nil {
		q.claimed <- *claim
	}
	return claim, err
}

func TestWorkerRecoveryEdges_IgnoredContextCannotRetireSuccessor(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	queue := &freshObservedClaimQueue{
		MemoryQueue: workflow.NewMemoryQueue(workflow.WithMemoryQueueNowFunc(clock.Now)),
		claimed:     make(chan workflow.ClaimedTask, 8),
	}
	runner, err := workflow.NewRunner(
		eventlog.NewMemory(),
		freshWorkerReviewLogger(),
		workflow.WithTaskQueue(queue),
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	bound, stopBound := context.WithTimeout(t.Context(), time.Second)
	defer stopBound()
	entered := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	wf := workflow.New(
		runner,
		"ignored-worker-context",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(context.Context) error {
				if attempts.Add(1) == 1 {
					close(entered)
					// Ignore only the worker context, not the test's independent bound.
					select {
					case <-release:
					case <-bound.Done():
						return bound.Err()
					}
				}
				return nil
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(bound)
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: time.Millisecond})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("old worker failed to join")
		}
	})
	select {
	case <-entered:
	case <-bound.Done():
		t.Fatal("callback did not enter")
	}
	old := <-queue.claimed
	cancel()
	require.NoError(
		t,
		queue.Enqueue(
			t.Context(),
			workflow.QueuedTask{InstanceID: id, WorkflowName: "ignored-worker-context"},
		),
	)
	clock.Advance(time.Minute)
	current, err := queue.PollClaim(t.Context())
	require.NoError(t, err)
	require.NotNil(t, current, "cancellation must stop renewal even while the callback ignores it")
	require.NotEqual(t, old.Receipt, current.Receipt)
	_, err = wf.Run(bound, id)
	require.NoError(t, err)
	close(release)
	select {
	case <-exited:
	case <-bound.Done():
		t.Fatal("old worker did not join after its callback returned")
	}
	require.NoError(
		t,
		queue.ExtendClaim(t.Context(), *current, time.Minute),
		"old-worker cleanup must not retire the successor receipt",
	)
	require.NoError(t, queue.CompleteClaim(t.Context(), *current))
	require.ErrorIs(t, queue.ReleaseClaim(t.Context(), old, time.Time{}), workflow.ErrStaleClaim)
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(t.Context(), id)
		return err == nil
	}, 100*time.Millisecond, time.Millisecond)
	require.Equal(t, int32(2), attempts.Load())
}

func TestWorkerRecoveryEdges_UnknownDefinitionDoesNotStarveRunnableWork(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "async"}[async], func(t *testing.T) {
			var queue workflow.ClaimedTaskQueue = workflow.NewMemoryQueue()
			if async {
				q, err := workflow.NewAsyncQueue(
					t.Context(),
					setupAsyncDB(t),
					workflow.WithAsyncQueueWorkerID("same-label"),
				)
				require.NoError(t, err)
				queue = q
			}
			require.NoError(
				t,
				queue.Enqueue(
					t.Context(),
					workflow.QueuedTask{
						InstanceID:   "unknown-first",
						WorkflowName: "not-registered",
					},
				),
			)
			runner, err := workflow.NewRunner(
				eventlog.NewMemory(),
				freshWorkerReviewLogger(),
				workflow.WithTaskQueue(queue),
			)
			require.NoError(t, err)
			wf := workflow.New(
				runner,
				"runnable-after-unknown",
				func(*workflow.Context, *struct{}) (*struct{}, error) {
					return &struct{}{}, nil
				},
			)
			id, err := wf.Start(t.Context(), &struct{}{})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			err = runner.RunWorker(
				ctx,
				workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1},
			)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			_, err = wf.GetResult(t.Context(), id)
			require.NoError(t, err, "an unknown definition must not block a runnable one")
			var claim *workflow.ClaimedTask
			require.Eventually(t, func() bool {
				claim, err = queue.PollClaim(t.Context())
				return err == nil && claim != nil
			}, 100*time.Millisecond, time.Millisecond, "unknown definition must remain recoverable after its retry delay")
			require.Equal(t, workflow.InstanceID("unknown-first"), claim.InstanceID)
		})
	}
}

var freshWorkerReviewDriverSequence atomic.Uint64

func TestWorkerRecoveryEdges_AsyncNameReadFailureMustNotPoisonTask(t *testing.T) {
	var denyNameReads atomic.Int32
	driverName := fmt.Sprintf("fresh-worker-review-%d", freshWorkerReviewDriverSequence.Add(1))
	sql.Register(
		driverName,
		&sqlite3.SQLiteDriver{Extensions: nil, ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			conn.RegisterAuthorizer(func(op int, table, column, _ string) int {
				if op == sqlite3.SQLITE_READ && table == "workflow_async_names" &&
					column == "workflow_name" &&
					denyNameReads.Load() > 0 {
					denyNameReads.Add(-1)
					return sqlite3.SQLITE_DENY
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}},
	)
	db, err := sql.Open(driverName, filepath.Join(t.TempDir(), "async-name.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	queue, err := workflow.NewAsyncQueue(t.Context(), db)
	require.NoError(t, err)
	log := eventlog.NewMemory()
	runner, err := workflow.NewRunner(log, freshWorkerReviewLogger(), workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"async-timed-wait",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			_, err := workflow.WaitForEvent[string](
				wctx,
				"fresh:approval",
				workflow.WaitForEventOptions{Timeout: 5 * time.Millisecond},
			)
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	for rec, err := range log.ReadAllEvents(t.Context(), version.SelectFromBeginning) {
		require.NoError(t, err)
		require.NoError(t, queue.Handle(t.Context(), rec))
	}
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	var waitRecord *event.GlobalRecord
	for rec, err := range log.ReadAllEvents(t.Context(), version.SelectFromBeginning) {
		require.NoError(t, err)
		if rec.EventName() == "workflow/waiting" {
			waitRecord = rec
		}
	}
	require.NotNil(t, waitRecord)

	// Old waiting events omit the name, so they must read retained identity.
	var legacyPayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(waitRecord.Data(), &legacyPayload))
	delete(legacyPayload, "workflowName")
	encoded, err := json.Marshal(legacyPayload)
	require.NoError(t, err)
	legacyWait := event.NewGlobalRecord(
		waitRecord.GlobalVersion(),
		waitRecord.Version(),
		waitRecord.LogID(),
		waitRecord.EventName(),
		encoded,
	)
	denyNameReads.Store(1)
	handleErr := queue.Handle(t.Context(), legacyWait)
	require.Zero(t, denyNameReads.Load(), "the identity read must fail")
	require.Error(t, handleErr, "projection errors must prevent checkpoint advancement")
	require.NoError(
		t,
		queue.Handle(t.Context(), legacyWait),
		"retry must succeed after storage recovers",
	)
	var scheduledName string
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), "SELECT workflow_name FROM workflow_ready_tasks WHERE instance_id = ?", string(id)).
			Scan(&scheduledName),
	)
	require.Equal(t, "async-timed-wait", scheduledName)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err = runner.RunWorker(
		ctx,
		workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(
		t,
		workflow.StatusFailed,
		info.Status,
		"the timed wait must resume its timeout decision after storage recovery",
	)
}

func TestWorkerRecoveryEdges_AsyncLegacyNameSurvivesTaskRetirement(t *testing.T) {
	for _, source := range []string{"started", "embedded-step"} {
		t.Run(source, func(t *testing.T) {
			db := setupAsyncDB(t)
			queue, err := workflow.NewAsyncQueue(t.Context(), db)
			require.NoError(t, err)
			const id event.LogID = "legacy-retired-waiter"
			const name = "retained-definition"
			if source == "started" {
				rec := event.NewGlobalRecord(
					1,
					1,
					id,
					"workflow/started",
					[]byte(
						`{"instanceID":"legacy-retired-waiter","workflowName":"retained-definition","params":{}}`,
					),
				)
				require.NoError(t, queue.Handle(t.Context(), rec))
				claim, err := queue.PollClaim(t.Context())
				require.NoError(t, err)
				require.NotNil(t, claim)
				require.NoError(t, queue.CompleteClaim(t.Context(), *claim))
			} else {
				rec := event.NewGlobalRecord(1, 1, id, "workflow/step_completed", []byte(`{"stepIndex":0,"result":null,"kind":"step","workflowName":"retained-definition"}`))
				require.NoError(t, queue.Handle(t.Context(), rec))
			}
			require.Zero(t, queue.Len(), "identity must not require a ready-task row")
			// A new queue object also rules out an in-memory name cache.
			queue, err = workflow.NewAsyncQueue(t.Context(), db)
			require.NoError(t, err)
			legacyWait := event.NewGlobalRecord(
				2,
				2,
				id,
				"workflow/waiting",
				[]byte(
					`{"stepIndex":1,"awaitingEvent":"legacy:approval","deadline":"2020-01-01T00:00:00Z"}`,
				),
			)
			require.NoError(t, queue.Handle(t.Context(), legacyWait))
			claim, err := queue.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, claim)
			require.Equal(t, name, claim.WorkflowName)
			require.NoError(t, queue.CompleteClaim(t.Context(), *claim))
			terminal := event.NewGlobalRecord(
				3,
				3,
				id,
				"workflow/completed",
				[]byte(`{"result":null}`),
			)
			require.NoError(t, queue.Handle(t.Context(), terminal))
			require.NoError(
				t,
				queue.Handle(t.Context(), legacyWait),
				"terminal cleanup must not delete retained identity",
			)
			claim, err = queue.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, claim)
			require.Equal(t, name, claim.WorkflowName)
		})
	}
}

func TestWorkerRecoveryEdges_AsyncCurrentWaitBypassesDisposableIdentity(t *testing.T) {
	var denyTaskReads atomic.Bool
	driverName := fmt.Sprintf("worker-recovery-current-%d", freshWorkerReviewDriverSequence.Add(1))
	sql.Register(
		driverName,
		&sqlite3.SQLiteDriver{Extensions: nil, ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			conn.RegisterAuthorizer(func(op int, table, column, _ string) int {
				if op == sqlite3.SQLITE_READ && table == "workflow_ready_tasks" &&
					column == "workflow_name" &&
					denyTaskReads.Load() {
					return sqlite3.SQLITE_DENY
				}
				return sqlite3.SQLITE_OK
			})
			return nil
		}},
	)
	db, err := sql.Open(driverName, filepath.Join(t.TempDir(), "current-name.db"))
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	queue, err := workflow.NewAsyncQueue(t.Context(), db)
	require.NoError(t, err)
	denyTaskReads.Store(true)
	// A failed disposable-row read must not prevent a current event from routing.
	rec := event.NewGlobalRecord(
		1,
		1,
		"current-waiter",
		"workflow/waiting",
		[]byte(
			`{"stepIndex":0,"workflowName":"current-definition","awaitingEvent":"current:approval","deadline":"2020-01-01T00:00:00Z"}`,
		),
	)
	require.NoError(t, queue.Handle(t.Context(), rec))
	denyTaskReads.Store(false)
	claim, err := queue.PollClaim(t.Context())
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, "current-definition", claim.WorkflowName)
}
