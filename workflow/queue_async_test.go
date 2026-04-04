package workflow_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// setupAsyncDB creates a file-backed SQLite database for async queue tests.
func setupAsyncDB(t *testing.T) *sql.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "async-queue-*.db")
	require.NoError(t, err)
	db, err := sql.Open("sqlite3", f.Name())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// memoryCheckpointer is a simple in-memory checkpointer for testing.
type memoryCheckpointer struct {
	versions map[string]version.Version
}

func newMemoryCheckpointer() *memoryCheckpointer {
	return &memoryCheckpointer{versions: make(map[string]version.Version)}
}

func (m *memoryCheckpointer) GetCheckpoint(_ context.Context, projectionName string) (version.Version, error) {
	v, ok := m.versions[projectionName]
	if !ok {
		return version.Zero, nil
	}
	return v, nil
}

func (m *memoryCheckpointer) SaveCheckpoint(_ context.Context, projectionName string, v version.Version) error {
	m.versions[projectionName] = v
	return nil
}

// setupAsyncRunner creates a Runner with an in-memory event log + AsyncQueue +
// AsyncProjectionRunner, simulating the non-transactional scenario.
func setupAsyncRunner(t *testing.T, opts ...workflow.RunnerOption) (*workflow.Runner, func()) {
	t.Helper()

	// Use a file-based DB for the async queue tables (separate from the event log).
	db := setupAsyncDB(t)

	asyncQueue, err := workflow.NewAsyncQueue(db)
	require.NoError(t, err)

	// Prepend the queue option so user opts can override.
	allOpts := append([]workflow.RunnerOption{workflow.WithTaskQueue(asyncQueue)}, opts...)

	// In-memory event log (non-transactional, global-reader capable).
	memLog := eventlog.NewMemory()

	runner, err := workflow.NewRunner(memLog, nil, allOpts...)
	require.NoError(t, err)

	// Start the async projection runner in the background.
	projCtx, projCancel := context.WithCancel(context.Background())
	checkpointer := newMemoryCheckpointer()
	projRunner, err := event.NewAsyncProjectionRunner(
		memLog,
		checkpointer,
		asyncQueue,
		"workflow-async-queue",
		event.WithPollInterval(20*time.Millisecond),
	)
	require.NoError(t, err)

	go func() {
		_ = projRunner.Run(projCtx)
	}()

	cleanup := func() {
		projCancel()
	}

	return runner, cleanup
}

func TestAsyncQueue_SimpleWorkflowCompletion(t *testing.T) {
	runner, cleanup := setupAsyncRunner(t)
	defer cleanup()

	wf := workflow.New(runner, "simple-async", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "hello-" + params.Value, nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: val}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "async"})
	require.NoError(t, err)

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "hello-async"
	}, 5*time.Second, 50*time.Millisecond)

	cancel()
	<-done
}

func TestAsyncQueue_SleepAndWakeUp(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	runner, cleanup := setupAsyncRunner(t, workflow.WithNowFunc(clock.Now))
	defer cleanup()

	wf := workflow.New(runner, "sleep-async", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "before", nil
		})
		if err != nil {
			return nil, err
		}

		if err := workflow.Sleep(wctx, 2*time.Hour); err != nil {
			return nil, err
		}

		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "after-sleep", nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	// Let the worker pick up the task and hit sleep.
	time.Sleep(300 * time.Millisecond)

	_, err = wf.GetResult(ctx, instanceID)
	require.Error(t, err, "workflow should not be complete yet (sleeping)")

	// Advance clock past the sleep.
	clock.Advance(3 * time.Hour)

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "after-sleep"
	}, 5*time.Second, 50*time.Millisecond)

	cancel()
	<-done
}

func TestAsyncQueue_RetryOnFailure(t *testing.T) {
	runner, cleanup := setupAsyncRunner(t)
	defer cleanup()

	var attempts atomic.Int32

	wf := workflow.New(runner, "retry-async", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			n := attempts.Add(1)
			if n < 3 {
				return "", errors.New("transient error")
			}
			return "success", nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: "done"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "retry"}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Millisecond, // tiny delays for testing
		Factor:      1.0,
		MaxDelay:    10 * time.Millisecond,
	}))
	require.NoError(t, err)

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "done"
	}, 5*time.Second, 50*time.Millisecond)

	require.GreaterOrEqual(t, int(attempts.Load()), 3)

	cancel()
	<-done
}

func TestAsyncQueue_MultipleWorkflows(t *testing.T) {
	runner, cleanup := setupAsyncRunner(t)
	defer cleanup()

	wf := workflow.New(runner, "multi-async", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "result-" + params.Value, nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: val}, nil
	})

	ctx := t.Context()
	const n = 5
	ids := make([]workflow.InstanceID, n)
	for i := range n {
		id, err := wf.Start(ctx, &WorkerTestParams{Value: string(rune('a' + i))})
		require.NoError(t, err)
		ids[i] = id
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	// All workflows should complete.
	for _, id := range ids {
		id := id
		require.Eventually(t, func() bool {
			result, err := wf.GetResult(ctx, id)
			return err == nil && result != nil
		}, 5*time.Second, 50*time.Millisecond, "workflow %s did not complete", id)
	}

	cancel()
	<-done
}

func TestAsyncQueue_MatchesEventFilters(t *testing.T) {
	db := setupAsyncDB(t)
	q, err := workflow.NewAsyncQueue(db)
	require.NoError(t, err)

	require.True(t, q.MatchesEvent("workflow/started"))
	require.True(t, q.MatchesEvent("workflow/step_completed"))
	require.True(t, q.MatchesEvent("workflow/completed"))
	require.True(t, q.MatchesEvent("workflow/failed"))
	require.True(t, q.MatchesEvent("workflow/retried"))
	require.True(t, q.MatchesEvent("workflow/waiting"))
	require.True(t, q.MatchesEvent("workflow/event_received"))
	require.True(t, q.MatchesEvent("workflow/cancelled"))
	require.False(t, q.MatchesEvent("account/opened"))
	require.False(t, q.MatchesEvent("unknown"))
}
