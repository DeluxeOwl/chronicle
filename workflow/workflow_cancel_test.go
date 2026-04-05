package workflow_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestCancelWorkflow_ManualCancel(t *testing.T) {
	db := setupWorkerDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	stepReached := make(chan struct{})
	proceed := make(chan struct{})

	wf := workflow.New(
		runner,
		"cancellable",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				close(stepReached)
				<-proceed // Block until test signals to continue
				return "done", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Cancel before the worker picks it up.
	err = workflow.CancelWorkflow(ctx, runner, instanceID, "user requested")
	require.NoError(t, err)

	// Verify status is cancelled.
	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, info.Status)

	// Run should return ErrWorkflowCancelled.
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowCancelled)

	close(proceed)
}

func TestCancelWorkflow_Idempotent(t *testing.T) {
	db := setupWorkerDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"cancel-idem",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			return &WorkerTestOutput{Result: "ok"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Cancel the first time.
	err = workflow.CancelWorkflow(ctx, runner, instanceID, "first cancel")
	require.NoError(t, err)

	// Cancel again — should be a no-op.
	err = workflow.CancelWorkflow(ctx, runner, instanceID, "second cancel")
	require.NoError(t, err)

	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, info.Status)
}

func TestCancelWorkflow_CompletedIsNoop(t *testing.T) {
	db := setupWorkerDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"completed-cancel",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Run to completion.
	_, err = wf.Run(ctx, instanceID)
	require.NoError(t, err)

	// Cancel should be a no-op.
	err = workflow.CancelWorkflow(ctx, runner, instanceID, "too late")
	require.NoError(t, err)

	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCompleted, info.Status)
}

func TestCancelWorkflow_WorkerHandlesCancelled(t *testing.T) {
	db := setupWorkerDB(t)
	runner, err := workflow.NewSqliteRunner(db)
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"worker-cancel",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-1", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Cancel before worker picks it up.
	err = workflow.CancelWorkflow(ctx, runner, instanceID, "cancelled before worker")
	require.NoError(t, err)

	// Run worker — it should handle the cancelled workflow gracefully.
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	// Give worker time to process the task.
	time.Sleep(200 * time.Millisecond)

	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, info.Status)

	cancel()
	<-done
}

func TestCancellationPolicy_MaxDuration(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupWorkerDB(t)

	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"max-duration",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-1", nil
			})
			if err != nil {
				return nil, err
			}

			if err := workflow.Sleep(wctx, 30*time.Minute); err != nil {
				return nil, err
			}

			_, err = workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-2", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"},
		workflow.WithCancellationPolicy(workflow.CancellationPolicy{
			MaxDuration: 1 * time.Hour,
		}),
	)
	require.NoError(t, err)

	// Run once — should hit sleep.
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance clock past MaxDuration.
	clock.Advance(2 * time.Hour)

	// Run again — should be auto-cancelled.
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowCancelled)

	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, info.Status)
}

func TestCancellationPolicy_MaxDelay(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupWorkerDB(t)

	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"max-delay",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-1", nil
			})
			if err != nil {
				return nil, err
			}

			if err := workflow.Sleep(wctx, 30*time.Minute); err != nil {
				return nil, err
			}

			_, err = workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-2", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"},
		workflow.WithCancellationPolicy(workflow.CancellationPolicy{
			MaxDelay: 45 * time.Minute,
		}),
	)
	require.NoError(t, err)

	// Run once — step-1 completes, then hits sleep.
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance clock past the sleep but also past MaxDelay since last checkpoint.
	clock.Advance(50 * time.Minute)

	// Run again — should be auto-cancelled because no new step for > 45 minutes.
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowCancelled)

	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, info.Status)
}

func TestCancellationPolicy_WithinLimitsCompletes(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupWorkerDB(t)

	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"within-limits",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-1", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"},
		workflow.WithCancellationPolicy(workflow.CancellationPolicy{
			MaxDuration: 1 * time.Hour,
			MaxDelay:    30 * time.Minute,
		}),
	)
	require.NoError(t, err)

	// Run within limits.
	clock.Advance(5 * time.Minute)
	result, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "done", result.Result)
}

func TestCancellationPolicy_WorkerAutoCancel(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupSyncDB(t)

	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	var stepCount atomic.Int32

	wf := workflow.New(
		runner,
		"worker-auto-cancel",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				stepCount.Add(1)
				return "step-1", nil
			})
			if err != nil {
				return nil, err
			}

			if err := workflow.Sleep(wctx, 30*time.Minute); err != nil {
				return nil, err
			}

			_, err = workflow.Step(wctx, func(ctx context.Context) (string, error) {
				stepCount.Add(1)
				return "step-2", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"},
		workflow.WithCancellationPolicy(workflow.CancellationPolicy{
			MaxDuration: 1 * time.Hour,
		}),
	)
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

	// Let worker process step-1 and hit sleep.
	time.Sleep(200 * time.Millisecond)
	require.Equal(t, int32(1), stepCount.Load())

	// Advance clock past MaxDuration.
	clock.Advance(2 * time.Hour)

	// Worker should pick up the delayed task and auto-cancel.
	require.Eventually(t, func() bool {
		info, err := wf.GetStatus(ctx, instanceID)
		return err == nil && info.Status == workflow.StatusCancelled
	}, 5*time.Second, 50*time.Millisecond)

	// Step-2 should NOT have been executed.
	require.Equal(t, int32(1), stepCount.Load())

	cancel()
	<-done
}

func TestCancelWorkflow_SyncQueueCleansUpTask(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf := workflow.New(
		runner,
		"sync-cancel-cleanup",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "step-1", nil
			})
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Cancel the workflow.
	err = workflow.CancelWorkflow(ctx, runner, instanceID, "cleanup test")
	require.NoError(t, err)

	// Verify the task was cleaned up from the queue.
	// The SyncQueue.Process should have deleted the task on workflowCancelled.
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM workflow_ready_tasks WHERE instance_id = ?`, string(instanceID)).
		Scan(&count)
	require.NoError(t, err)
	require.Equal(t, 0, count)
}
