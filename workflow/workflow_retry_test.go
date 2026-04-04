package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func setupRetryDB(t *testing.T) *setupResult {
	t.Helper()
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	return &setupResult{runner: runner, clock: clock}
}

type setupResult struct {
	runner *workflow.Runner
	clock  *controllableClock
}

type RetryTestParams struct {
	Value string `json:"value"`
}

type RetryTestOutput struct {
	Result string `json:"result"`
}

func TestRetry_SucceedsAfterTransientFailure(t *testing.T) {
	s := setupRetryDB(t)

	var attempts atomic.Int32

	wf := workflow.New(s.runner, "retry-transient", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		n := attempts.Add(1)
		if n < 3 {
			return nil, errors.New("transient error")
		}
		return &RetryTestOutput{Result: "success-on-attempt-3"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{Value: "test"}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		Factor:      2.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Attempt 1: fails, schedules retry
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, int32(1), attempts.Load())

	// Still not ready (need to advance past backoff delay)
	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, info.Status)
	require.Equal(t, 2, info.Attempt)

	// Advance clock past first backoff (1s)
	s.clock.Advance(2 * time.Second)

	// Attempt 2: fails, schedules retry
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, int32(2), attempts.Load())

	// Advance clock past second backoff (2s)
	s.clock.Advance(3 * time.Second)

	// Attempt 3: succeeds
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "success-on-attempt-3", output.Result)
	require.Equal(t, int32(3), attempts.Load())

	// GetResult works
	result, err := wf.GetResult(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "success-on-attempt-3", result.Result)
}

func TestRetry_ExhaustsMaxAttempts(t *testing.T) {
	s := setupRetryDB(t)

	var attempts atomic.Int32

	wf := workflow.New(s.runner, "retry-exhaust", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		attempts.Add(1)
		return nil, errors.New("permanent error")
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{Value: "test"}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		Factor:      1.0, // fixed delay
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Attempt 1: fails, schedules retry (attempt 2)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(2 * time.Second)

	// Attempt 2: fails, schedules retry (attempt 3)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(2 * time.Second)

	// Attempt 3: fails, max attempts exhausted → permanent failure
	_, err = wf.Run(ctx, instanceID)
	require.Error(t, err)
	require.NotErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Contains(t, err.Error(), "permanent error")

	// Workflow should be in failed state
	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusFailed, info.Status)

	require.Equal(t, int32(3), attempts.Load())
}

func TestRetry_NoStrategyMeansNoRetry(t *testing.T) {
	s := setupRetryDB(t)

	var attempts atomic.Int32

	wf := workflow.New(s.runner, "retry-none", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		attempts.Add(1)
		return nil, errors.New("boom")
	})

	ctx := t.Context()
	// No WithRetryStrategy — should fail immediately
	instanceID, err := wf.Start(ctx, &RetryTestParams{Value: "test"})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.Error(t, err)
	require.NotErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Contains(t, err.Error(), "boom")

	info, err := wf.GetStatus(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusFailed, info.Status)

	require.Equal(t, int32(1), attempts.Load())
}

func TestRetry_PreservesCompletedSteps(t *testing.T) {
	s := setupRetryDB(t)

	var step1Count, step2Count atomic.Int32

	wf := workflow.New(s.runner, "retry-preserve-steps", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		// Step 1: always succeeds
		val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			step1Count.Add(1)
			return "step1-done", nil
		})
		if err != nil {
			return nil, err
		}

		// Step 2: fails on first attempt, succeeds on second
		err = workflow.Step2(wctx, func(ctx context.Context) error {
			n := step2Count.Add(1)
			if n == 1 {
				return errors.New("step2 transient failure")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}

		return &RetryTestOutput{Result: val + "-complete"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{Value: "test"}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Attempt 1: step 1 succeeds, step 2 fails, schedules retry
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, int32(1), step1Count.Load())
	require.Equal(t, int32(1), step2Count.Load())

	s.clock.Advance(2 * time.Second)

	// Attempt 2: step 1 is replayed (NOT re-executed), step 2 succeeds
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "step1-done-complete", output.Result)
	require.Equal(t, int32(1), step1Count.Load(), "step 1 should NOT re-execute (cached)")
	require.Equal(t, int32(2), step2Count.Load(), "step 2 should re-execute")
}

func TestRetry_SurvivesCrash(t *testing.T) {
	// Simulate a crash by creating a new runner against the same DB.
	// The retry strategy should be recovered from the event log.
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	runner1, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	var attempts atomic.Int32

	wf1 := workflow.New(runner1, "crash-retry", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		n := attempts.Add(1)
		if n < 2 {
			return nil, errors.New("fail before crash")
		}
		return &RetryTestOutput{Result: "recovered"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf1.Start(ctx, &RetryTestParams{Value: "test"}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		Factor:      2.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Attempt 1: fails, schedules retry
	_, err = wf1.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	// Simulate crash: create new runner, same DB
	clock.Advance(2 * time.Second)
	runner2, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	wf2 := workflow.New(runner2, "crash-retry", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		n := attempts.Add(1)
		if n < 2 {
			return nil, errors.New("fail before crash")
		}
		return &RetryTestOutput{Result: "recovered"}, nil
	})

	// Attempt 2: the retry strategy was recovered from the event log, workflow succeeds
	output, err := wf2.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "recovered", output.Result)
}

func TestRetry_ExponentialBackoff(t *testing.T) {
	s := setupRetryDB(t)

	var attempts atomic.Int32

	wf := workflow.New(s.runner, "retry-backoff", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		n := attempts.Add(1)
		if n < 5 {
			return nil, errors.New("still failing")
		}
		return &RetryTestOutput{Result: "done"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 10,
		BaseDelay:   1 * time.Second,
		Factor:      2.0,
		MaxDelay:    30 * time.Second,
	}))
	require.NoError(t, err)

	// Attempt 1 → fail → delay = 1s
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(2 * time.Second)

	// Attempt 2 → fail → delay = 2s
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(3 * time.Second)

	// Attempt 3 → fail → delay = 4s
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(5 * time.Second)

	// Attempt 4 → fail → delay = 8s
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(9 * time.Second)

	// Attempt 5 → success
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "done", output.Result)
	require.Equal(t, int32(5), attempts.Load())
}

func TestRetry_WorkerDrivesRetries(t *testing.T) {
	db := setupTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	runner, err := workflow.NewSqliteRunner(db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)

	var attempts atomic.Int32

	wf := workflow.New(runner, "worker-retry", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		n := attempts.Add(1)
		if n < 3 {
			return nil, errors.New("transient")
		}
		return &RetryTestOutput{Result: "worker-retried"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{Value: "test"}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond, // small delays for testing
		Factor:      1.0,
		MaxDelay:    1 * time.Second,
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

	// Worker picks up the task, it fails, retry is scheduled.
	// Advance clock to allow retries to become ready.
	require.Eventually(t, func() bool {
		// Keep advancing the clock to let retries fire
		clock.Advance(200 * time.Millisecond)
		result, err := wf.GetResult(ctx, instanceID)
		return err == nil && result.Result == "worker-retried"
	}, 5*time.Second, 20*time.Millisecond)

	require.Equal(t, int32(3), attempts.Load())

	cancel()
	<-done
}

func TestRetry_WithSleep(t *testing.T) {
	// Workflow: step → sleep → step that fails → retry → sleep replays → step succeeds
	s := setupRetryDB(t)

	var failingStepCount atomic.Int32

	wf := workflow.New(s.runner, "retry-with-sleep", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			return "before-sleep", nil
		})
		if err != nil {
			return nil, err
		}

		if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
			return nil, err
		}

		// This step fails on first attempt after sleep
		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			n := failingStepCount.Add(1)
			if n == 1 {
				return "", errors.New("post-sleep failure")
			}
			return "after-sleep-retry", nil
		})
		if err != nil {
			return nil, err
		}

		return &RetryTestOutput{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Run 1: step 1 succeeds, hits sleep
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance past sleep
	s.clock.Advance(2 * time.Hour)

	// Run 2: sleep replays (elapsed), failing step executes and fails → retry
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, int32(1), failingStepCount.Load())

	// Advance past retry delay
	s.clock.Advance(2 * time.Second)

	// Run 3: replays step 1, replays sleep (elapsed), failing step succeeds
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "after-sleep-retry", output.Result)
}

func TestRetry_MaxDelayClamps(t *testing.T) {
	s := setupRetryDB(t)

	var attempts atomic.Int32

	wf := workflow.New(s.runner, "retry-clamp", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		n := attempts.Add(1)
		if n < 6 {
			return nil, errors.New("failing")
		}
		return &RetryTestOutput{Result: "clamped"}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 10,
		BaseDelay:   1 * time.Second,
		Factor:      10.0,        // aggressive factor
		MaxDelay:    5 * time.Second, // but clamped to 5s
	}))
	require.NoError(t, err)

	for range 5 {
		_, err = wf.Run(ctx, instanceID)
		require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
		// Always advance enough to clear max delay
		s.clock.Advance(6 * time.Second)
	}

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "clamped", output.Result)
}

func TestRetry_DefaultStrategy(t *testing.T) {
	rs := workflow.DefaultRetryStrategy()
	require.Equal(t, 5, rs.MaxAttempts)
	require.Equal(t, 1*time.Second, rs.BaseDelay)
	require.Equal(t, 2.0, rs.Factor)
	require.Equal(t, 5*time.Minute, rs.MaxDelay)
}

func TestRetry_MultipleWorkflowsDifferentStrategies(t *testing.T) {
	s := setupRetryDB(t)

	var attemptsA, attemptsB atomic.Int32

	type Out struct {
		Val string `json:"val"`
	}

	wfA := workflow.New(s.runner, "retry-a", func(wctx *workflow.Context, params *RetryTestParams) (*Out, error) {
		n := attemptsA.Add(1)
		if n < 2 {
			return nil, errors.New("a fails once")
		}
		return &Out{Val: "a-ok"}, nil
	})

	wfB := workflow.New(s.runner, "retry-b", func(wctx *workflow.Context, params *RetryTestParams) (*Out, error) {
		attemptsB.Add(1)
		return nil, errors.New("b always fails")
	})

	ctx := t.Context()

	idA, err := wfA.Start(ctx, &RetryTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	idB, err := wfB.Start(ctx, &RetryTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 2,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// A: attempt 1 fails → retry
	_, err = wfA.Run(ctx, idA)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	// B: attempt 1 fails → retry
	_, err = wfB.Run(ctx, idB)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)

	s.clock.Advance(2 * time.Second)

	// A: attempt 2 succeeds
	outA, err := wfA.Run(ctx, idA)
	require.NoError(t, err)
	require.Equal(t, "a-ok", outA.Val)

	// B: attempt 2 fails → exhausted
	_, err = wfB.Run(ctx, idB)
	require.Error(t, err)
	require.NotErrorIs(t, err, workflow.ErrWorkflowRetrying)

	infoB, err := wfB.GetStatus(ctx, idB)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusFailed, infoB.Status)
}

func TestRetry_StepFailureRecordedButStepReExecutesOnRetry(t *testing.T) {
	// This verifies that a stepFailed event does NOT cache the result,
	// so the step re-executes on retry.
	s := setupRetryDB(t)

	var stepExecCount atomic.Int32

	wf := workflow.New(s.runner, "retry-step-reexec", func(wctx *workflow.Context, params *RetryTestParams) (*RetryTestOutput, error) {
		result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
			n := stepExecCount.Add(1)
			if n == 1 {
				return "", errors.New("step fails first time")
			}
			return fmt.Sprintf("success-attempt-%d", n), nil
		})
		if err != nil {
			return nil, err
		}
		return &RetryTestOutput{Result: result}, nil
	})

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &RetryTestParams{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		Factor:      1.0,
		MaxDelay:    1 * time.Minute,
	}))
	require.NoError(t, err)

	// Attempt 1: step fails
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, int32(1), stepExecCount.Load())

	s.clock.Advance(2 * time.Second)

	// Attempt 2: step re-executes and succeeds
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "success-attempt-2", output.Result)
	require.Equal(t, int32(2), stepExecCount.Load())
}
