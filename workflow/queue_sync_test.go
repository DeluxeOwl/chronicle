package workflow_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func setupSyncDB(t *testing.T) *sql.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "sync-queue-*.db")
	require.NoError(t, err)
	db, err := sql.Open("sqlite3", f.Name())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSyncQueue_SimpleWorkflowCompletion(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "simple-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "hello-" + params.Value, nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: val}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "world"})
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
		return err == nil && result.Result == "hello-world"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_SleepAndWakeUp(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupSyncDB(t)

	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf := workflow.New(runner, "sleep-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
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

	// Wait for the worker to hit the sleep
	time.Sleep(100 * time.Millisecond)
	_, err = wf.GetResult(ctx, instanceID)
	require.Error(t, err, "workflow should not be complete yet (sleeping)")

	// Advance clock past the sleep
	clock.Advance(3 * time.Hour)

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "after-sleep"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_RetryWithBackoff(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupSyncDB(t)

	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	var attempts atomic.Int32

	wf := workflow.New(runner, "retry-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			n := attempts.Add(1)
			if n < 3 {
				return "", fmt.Errorf("attempt %d failed", n)
			}
			return "success", nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: "done"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"},
		workflow.WithRetryStrategy(workflow.RetryStrategy{
			MaxAttempts: 5,
			BaseDelay:   1 * time.Second,
			Factor:      2.0,
			MaxDelay:    1 * time.Minute,
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

	// First attempt should run and fail, scheduling retry at +1s
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(1), attempts.Load())

	// Advance past first retry delay (1s)
	clock.Advance(2 * time.Second)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, int32(2), attempts.Load())

	// Advance past second retry delay (2s)
	clock.Advance(3 * time.Second)

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "done"
	}, 2*time.Second, 20*time.Millisecond)

	require.Equal(t, int32(3), attempts.Load())

	cancel()
	<-done
}

func TestSyncQueue_CrashRecovery(t *testing.T) {
	// This is the key test that differentiates SyncQueue from MemoryQueue.
	// After a "crash" (destroying the runner), a new runner with the same DB
	// picks up pending tasks from the persistent queue.
	db := setupSyncDB(t)
	ctx := t.Context()

	// --- Phase 1: Start a workflow, let it run the first step, then "crash" ---
	runner1, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	var step1Count atomic.Int32

	wf1 := workflow.New(runner1, "crash-test", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step1Count.Add(1)
			return "step1-done", nil
		})
		if err != nil {
			return nil, err
		}

		// Step 2 simulates a crash — the process dies before completing.
		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "step2-done", nil
		})
		if err != nil {
			return nil, err
		}

		return &WorkerTestOutput{Result: result}, nil
	})

	instanceID, err := wf1.Start(ctx, &WorkerTestParams{Value: "crash"})
	require.NoError(t, err)

	// Run the workflow directly — it completes both steps.
	output, err := wf1.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "step2-done", output.Result)

	// --- Phase 2: "Crash" and create a new runner with a fresh queue ---
	// Start a NEW workflow that hasn't been executed yet.
	instanceID2, err := wf1.Start(ctx, &WorkerTestParams{Value: "pending"})
	require.NoError(t, err)

	// "Crash" — discard runner1. The MemoryQueue would lose the task.
	// But SyncQueue persisted it in the DB.
	runner1 = nil //nolint:ineffassign

	// --- Phase 3: New runner picks up the pending task ---
	runner2, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf2 := workflow.New(runner2, "crash-test", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "step1-recovered", nil
		})
		if err != nil {
			return nil, err
		}

		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "step2-recovered", nil
		})
		if err != nil {
			return nil, err
		}

		return &WorkerTestOutput{Result: result}, nil
	})

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner2.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	// The new worker should pick up instanceID2 from the persistent queue.
	require.Eventually(t, func() bool {
		result, err := wf2.GetResult(ctx, instanceID2)
		return err == nil && result.Result == "step2-recovered"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_CrashRecovery_SleepSurvivesRestart(t *testing.T) {
	// Workflow sleeps, process "crashes", new runner picks up the delayed task.
	clock := newClock(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	db := setupSyncDB(t)
	ctx := t.Context()

	// Phase 1: Start workflow that sleeps
	runner1, err := workflow.NewSqliteRunnerWithSyncQueue(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf1 := workflow.New(runner1, "sleep-crash", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: "woke-up"}, nil
	})

	instanceID, err := wf1.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Run via worker to let it hit the sleep and enqueue a delayed task
	worker1Ctx, cancel1 := context.WithCancel(ctx)
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		_ = runner1.RunWorker(worker1Ctx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	// Wait until the sleep step has been recorded and the delayed task
	// is in the DB (run_after_ns > 0 means it's a delayed wake-up, not
	// the immediate task from Start).
	require.Eventually(t, func() bool {
		var runAfterNs int64
		err := db.QueryRow(
			`SELECT run_after_ns FROM workflow_ready_tasks WHERE instance_id = ?`,
			string(instanceID),
		).Scan(&runAfterNs)
		return err == nil && runAfterNs > 0
	}, 2*time.Second, 10*time.Millisecond)

	cancel1()
	<-done1

	// Verify not complete yet
	_, err = wf1.GetResult(ctx, instanceID)
	require.Error(t, err)

	// "Crash" — discard runner1.

	// Phase 2: Advance clock past sleep, create new runner.
	clock.Advance(2 * time.Hour)

	runner2, err := workflow.NewSqliteRunnerWithSyncQueue(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf2 := workflow.New(runner2, "sleep-crash", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: "woke-up"}, nil
	})

	worker2Ctx, cancel2 := context.WithCancel(ctx)
	defer cancel2()

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		_ = runner2.RunWorker(worker2Ctx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	require.Eventually(t, func() bool {
		result, err := wf2.GetResult(ctx, instanceID)
		return err == nil && result.Result == "woke-up"
	}, 2*time.Second, 20*time.Millisecond)

	cancel2()
	<-done2
}

func TestSyncQueue_MultipleInstances(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	var count atomic.Int32

	wf := workflow.New(runner, "multi-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			count.Add(1)
			return "done", nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: "done"}, nil
	})

	ctx := t.Context()
	const n = 10
	ids := make([]workflow.InstanceID, n)
	for i := range n {
		id, err := wf.Start(ctx, &WorkerTestParams{Value: fmt.Sprintf("item-%d", i)})
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

	require.Eventually(t, func() bool {
		return int(count.Load()) == n
	}, 2*time.Second, 20*time.Millisecond)

	for _, id := range ids {
		result, err := wf.GetResult(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "done", result.Result)
	}

	cancel()
	<-done
}

func TestSyncQueue_WorkflowFailureDoesNotCrashWorker(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wfFail := workflow.New(runner, "fail-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		return nil, errors.New("boom")
	})

	wfOK := workflow.New(runner, "ok-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		return &WorkerTestOutput{Result: "success"}, nil
	})

	ctx := t.Context()
	_, err = wfFail.Start(ctx, &WorkerTestParams{Value: "fail"})
	require.NoError(t, err)

	idOK, err := wfOK.Start(ctx, &WorkerTestParams{Value: "ok"})
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
		result, err := wfOK.GetResult(ctx, idOK)
		return err == nil && result.Result == "success"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_HeartbeatExtendsLease(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "heartbeat-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			// Simulate long-running work with heartbeats
			for range 3 {
				if err := workflow.Heartbeat(wctx, 5*time.Minute); err != nil {
					return "", err
				}
			}
			return "heartbeat-done", nil
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

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "heartbeat-done"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_AwaitEventAndEmit(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	type EventPayload struct {
		Data string `json:"data"`
	}

	wf := workflow.New(runner, "await-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		payload, err := workflow.AwaitEvent[EventPayload](wctx, "test-event:"+params.Value)
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: payload.Data}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "evt1"})
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

	// Wait for the workflow to hit AwaitEvent
	time.Sleep(100 * time.Millisecond)

	// Emit the event
	err = workflow.EmitEvent(ctx, runner, "test-event:evt1", EventPayload{Data: "arrived"})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "arrived"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_TaskCleanedUpAfterCompletion(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "cleanup-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		return &WorkerTestOutput{Result: "done"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Verify a task exists in the table
	var taskCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM workflow_ready_tasks WHERE instance_id = ?`, string(instanceID)).Scan(&taskCount)
	require.NoError(t, err)
	require.Equal(t, 1, taskCount, "task should be in the queue")

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	// Wait for completion
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(ctx, instanceID)
		return err == nil
	}, 2*time.Second, 20*time.Millisecond)

	// Verify task is cleaned up
	err = db.QueryRow(`SELECT COUNT(*) FROM workflow_ready_tasks WHERE instance_id = ?`, string(instanceID)).Scan(&taskCount)
	require.NoError(t, err)
	require.Equal(t, 0, taskCount, "task should be removed after completion")

	cancel()
	<-done
}

func TestSyncQueue_GracefulShutdown(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	_ = workflow.New(runner, "noop-sync", func(wctx *workflow.Context, params *struct{}) (*struct{}, error) {
		return &struct{}{}, nil
	})

	workerCtx, cancel := context.WithCancel(t.Context())

	workerErr := make(chan error, 1)
	go func() {
		workerErr <- runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-workerErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not shut down in time")
	}
}

func TestSyncQueue_MultipleWorkflowTypes(t *testing.T) {
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	type OutputA struct {
		Val string `json:"val"`
	}
	type OutputB struct {
		Num int `json:"num"`
	}

	wfA := workflow.New(runner, "type-a-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*OutputA, error) {
		return &OutputA{Val: "a-" + params.Value}, nil
	})

	wfB := workflow.New(runner, "type-b-sync", func(wctx *workflow.Context, params *struct{ N int }) (*OutputB, error) {
		return &OutputB{Num: params.N * 2}, nil
	})

	ctx := t.Context()
	idA, err := wfA.Start(ctx, &WorkerTestParams{Value: "hello"})
	require.NoError(t, err)
	idB, err := wfB.Start(ctx, &struct{ N int }{N: 21})
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
		r, err := wfA.GetResult(ctx, idA)
		return err == nil && r.Val == "a-hello"
	}, 2*time.Second, 20*time.Millisecond)

	require.Eventually(t, func() bool {
		r, err := wfB.GetResult(ctx, idB)
		return err == nil && r.Num == 42
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}

func TestSyncQueue_DirectRunStillWorks(t *testing.T) {
	// Verify that Run() (direct execution, not via worker) still works with SyncQueue.
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf := workflow.New(runner, "direct-sync", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "direct-" + params.Value, nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: val}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "exec"})
	require.NoError(t, err)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "direct-exec", output.Result)

	// Result should be retrievable
	output2, err := wf.GetResult(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "direct-exec", output2.Result)
}

func TestSyncQueue_ReplayAfterCrash(t *testing.T) {
	// Verify that step replay works across process restarts:
	// steps completed before the crash are NOT re-executed.
	db := setupSyncDB(t)
	ctx := t.Context()

	// Phase 1: Run a multi-step workflow to completion.
	runner1, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	step1Exec := 0
	step2Exec := 0

	_ = workflow.New(runner1, "replay-crash", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step1Exec++
			return "s1", nil
		})
		if err != nil {
			return nil, err
		}

		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step2Exec++
			return "s2", nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: result}, nil
	})

	// Start a workflow — the Start creates the task atomically.
	// Don't run it yet; just let the task sit in the queue.

	// Phase 2: "Crash" and create a new runner.
	runner2, err := workflow.NewSqliteRunnerWithSyncQueue(db)
	require.NoError(t, err)

	wf2 := workflow.New(runner2, "replay-crash", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step1Exec++
			return "s1", nil
		})
		if err != nil {
			return nil, err
		}

		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step2Exec++
			return "s2", nil
		})
		if err != nil {
			return nil, err
		}
		return &WorkerTestOutput{Result: result}, nil
	})

	// Start a fresh instance using runner2
	instanceID, err := wf2.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Run directly
	output, err := wf2.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "s2", output.Result)
	require.Equal(t, 1, step1Exec, "step1 should execute once (runner2)")
	require.Equal(t, 1, step2Exec, "step2 should execute once (runner2)")

	// Run again — should replay without re-executing steps
	output2, err := wf2.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "s2", output2.Result)
	require.Equal(t, 1, step1Exec, "step1 should NOT re-execute on replay")
	require.Equal(t, 1, step2Exec, "step2 should NOT re-execute on replay")
}

func TestSyncQueue_ExpiredLeaseReclaimable(t *testing.T) {
	// Verify that if a worker claims a task but its lease expires,
	// another worker can reclaim it.
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupSyncDB(t)

	// Create a queue with a very short lease
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)

	wf := workflow.New(runner, "lease-test", func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
		return &WorkerTestOutput{Result: "ok"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &WorkerTestParams{Value: "test"})
	require.NoError(t, err)

	// Manually claim the task by simulating a poll (the worker claimed it).
	_, err = db.Exec(`
		UPDATE workflow_ready_tasks
		SET claimed_by = 'dead-worker', claimed_until_ns = ?
		WHERE instance_id = ?
	`, clock.Now().Add(10*time.Second).UnixNano(), string(instanceID))
	require.NoError(t, err)

	// Verify it can't be polled yet (lease not expired)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = runner.RunWorker(workerCtx, workflow.WorkerOptions{
			PollInterval: 10 * time.Millisecond,
		})
	}()

	time.Sleep(100 * time.Millisecond)
	_, err = wf.GetResult(ctx, instanceID)
	require.Error(t, err, "should not be complete while lease is active")

	// Advance clock past the lease expiry
	clock.Advance(30 * time.Second)

	require.Eventually(t, func() bool {
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "ok"
	}, 2*time.Second, 20*time.Millisecond)

	cancel()
	<-done
}
