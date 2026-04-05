package workflow_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func setupSleepTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file::memory:?cache=shared")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func setupSleepRunner(t *testing.T, clock *controllableClock) *workflow.Runner {
	t.Helper()
	db := setupSleepTestDB(t)
	runner, err := workflow.NewSqliteRunner(db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	return runner
}

type SleepTestParams struct {
	Value string `json:"value" exhaustruct:"optional"`
}

type SleepTestOutput struct {
	Result string `json:"result"`
}

func TestSleep_BasicFlow(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"sleep-basic",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			// Step 1: do some work
			val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "before-sleep", nil
			})
			if err != nil {
				return nil, err
			}

			// Sleep for 24 hours
			if err := workflow.Sleep(wctx, 24*time.Hour); err != nil {
				return nil, err
			}

			// Step 2: after sleep
			result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return val + "-after-sleep", nil
			})
			if err != nil {
				return nil, err
			}

			return &SleepTestOutput{Result: result}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{Value: "test"})
	require.NoError(t, err)

	// First run — should hit sleep and return ErrWorkflowSleeping
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance clock past the sleep
	clock.Advance(25 * time.Hour)

	// Re-run — sleep has elapsed, workflow completes
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "before-sleep-after-sleep", output.Result)
}

func TestSleep_StillSleepingOnReplay(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"sleep-still",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			if err := workflow.Sleep(wctx, 72*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	// First run — starts sleeping
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance only 1 hour — still sleeping
	clock.Advance(1 * time.Hour)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance to just before wake time (71 more hours = 72 total)
	clock.Advance(70 * time.Hour)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance past wake time
	clock.Advance(2 * time.Hour)
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "done", output.Result)
}

func TestSleep_MultipleSleepsInSequence(t *testing.T) {
	clock := newClock(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	stepLog := []string{}

	wf := workflow.New(
		runner,
		"multi-sleep",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				stepLog = append(stepLog, "step-1")
				return "s1", nil
			})
			if err != nil {
				return nil, err
			}

			// First sleep: 1 hour
			if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
				return nil, err
			}

			_, err = workflow.Step(wctx, func(ctx context.Context) (string, error) {
				stepLog = append(stepLog, "step-2")
				return "s2", nil
			})
			if err != nil {
				return nil, err
			}

			// Second sleep: 3 days
			if err := workflow.Sleep(wctx, 72*time.Hour); err != nil {
				return nil, err
			}

			_, err = workflow.Step(wctx, func(ctx context.Context) (string, error) {
				stepLog = append(stepLog, "step-3")
				return "s3", nil
			})
			if err != nil {
				return nil, err
			}

			return &SleepTestOutput{Result: "all-done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	// Run 1: executes step-1, hits first sleep
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	require.Equal(t, []string{"step-1"}, stepLog)

	// Advance past first sleep
	clock.Advance(2 * time.Hour)

	// Run 2: replays step-1, passes first sleep, executes step-2, hits second sleep
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	require.Equal(t, []string{"step-1", "step-2"}, stepLog)

	// Advance past second sleep
	clock.Advance(73 * time.Hour)

	// Run 3: replays everything, passes second sleep, executes step-3, completes
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "all-done", output.Result)
	require.Equal(t, []string{"step-1", "step-2", "step-3"}, stepLog)
}

func TestSleep_LongDuration_Days(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"sleep-days",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			// Sleep for 30 days
			if err := workflow.Sleep(wctx, 30*24*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "woke-after-30-days"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Jump forward 30 days + 1 second
	clock.Advance(30*24*time.Hour + time.Second)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "woke-after-30-days", output.Result)
}

func TestSleep_LongDuration_Weeks(t *testing.T) {
	clock := newClock(time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"sleep-weeks",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			// Sleep for 2 weeks
			if err := workflow.Sleep(wctx, 14*24*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "woke-after-2-weeks"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance 14 days + 1 minute
	clock.Advance(14*24*time.Hour + time.Minute)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "woke-after-2-weeks", output.Result)
}

func TestSleep_SurvivesCrash(t *testing.T) {
	// This test simulates a process crash by creating a new runner
	// against the same database. The sleep state should be recovered
	// from the event log.
	db := setupSleepTestDB(t)
	clock := newClock(time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))

	runner1, err := workflow.NewSqliteRunner(db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)

	wf1 := workflow.New(
		runner1,
		"crash-test",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			if err := workflow.Sleep(wctx, 48*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "survived-crash"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf1.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf1.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Advance time past the sleep
	clock.Advance(49 * time.Hour)

	// "Restart" — create a new runner with the same DB
	runner2, err := workflow.NewSqliteRunner(db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)

	wf2 := workflow.New(
		runner2,
		"crash-test",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			if err := workflow.Sleep(wctx, 48*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "survived-crash"}, nil
		},
	)

	// Re-run — the sleep step is replayed, clock shows it elapsed
	output, err := wf2.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "survived-crash", output.Result)
}

func TestSleep_AlreadyCompletedWorkflow(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"already-done",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	clock.Advance(2 * time.Hour)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "done", output.Result)

	// Running again should return the cached result, not sleep again
	output2, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "done", output2.Result)
}

func TestSleep_WithStepsBeforeAndAfter(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	step1Count := 0
	step2Count := 0

	wf := workflow.New(
		runner,
		"steps-around-sleep",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			// Step before sleep
			val, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				step1Count++
				return "prepared", nil
			})
			if err != nil {
				return nil, err
			}

			// Sleep
			if err := workflow.Sleep(wctx, 6*time.Hour); err != nil {
				return nil, err
			}

			// Step after sleep — should use result from step before
			result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				step2Count++
				return val + "-processed", nil
			})
			if err != nil {
				return nil, err
			}

			return &SleepTestOutput{Result: result}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	// First run — step 1 executes, then sleep
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	require.Equal(t, 1, step1Count)
	require.Equal(t, 0, step2Count)

	clock.Advance(7 * time.Hour)

	// Second run — step 1 is replayed (not re-executed), step 2 executes
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "prepared-processed", output.Result)
	require.Equal(t, 1, step1Count, "step 1 should NOT re-execute (cached)")
	require.Equal(t, 1, step2Count, "step 2 should execute once")
}

func TestSleep_ZeroDuration(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"zero-sleep",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			// Sleep for 0 — should effectively be a no-op on replay
			if err := workflow.Sleep(wctx, 0); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "instant"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	// Even with zero duration, the first run records the sleep step
	// and returns ErrWorkflowSleeping.
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Re-run immediately — wake time is now, clock hasn't advanced but
	// wakeAt == startTime so now.Before(wakeAt) is false
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "instant", output.Result)
}

func TestSleep_SleepErrorDoesNotMarkWorkflowFailed(t *testing.T) {
	// Verify that a sleeping workflow is NOT marked as failed.
	// After wake, it should complete normally.
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"not-failed",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "ok"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Trying to get the result should say "not completed", NOT "failed"
	_, err = wf.GetResult(ctx, instanceID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not completed")
	// It should say status is "running", not "failed"
	require.NotContains(t, err.Error(), "failed")

	// Now wake up and complete
	clock.Advance(2 * time.Hour)
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "ok", output.Result)

	// GetResult should now work
	output2, err := wf.GetResult(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "ok", output2.Result)
}

func TestSleep_MultipleWorkflowsSleepingConcurrently(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"concurrent-sleep",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			if err := workflow.Sleep(wctx, 2*time.Hour); err != nil {
				return nil, err
			}
			return &SleepTestOutput{Result: "woke-" + params.Value}, nil
		},
	)

	ctx := t.Context()

	// Start 5 workflows
	ids := make([]workflow.InstanceID, 5)
	for i := range 5 {
		id, err := wf.Start(ctx, &SleepTestParams{Value: fmt.Sprintf("wf-%d", i)})
		require.NoError(t, err)
		ids[i] = id
	}

	// All should enter sleep
	for _, id := range ids {
		_, err := wf.Run(ctx, id)
		require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	}

	// Advance time past sleep
	clock.Advance(3 * time.Hour)

	// All should complete
	for i, id := range ids {
		output, err := wf.Run(ctx, id)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprintf("woke-wf-%d", i), output.Result)
	}
}

func TestSleep_Step2BeforeSleep(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	step2Executed := 0

	wf := workflow.New(
		runner,
		"step2-sleep",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				step2Executed++
				return nil
			})
			if err != nil {
				return nil, err
			}

			if err := workflow.Sleep(wctx, 12*time.Hour); err != nil {
				return nil, err
			}

			return &SleepTestOutput{Result: "done"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	require.Equal(t, 1, step2Executed)

	clock.Advance(13 * time.Hour)

	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "done", output.Result)
	require.Equal(t, 1, step2Executed, "step2 should not re-execute")
}

func TestSleep_StepFailureBeforeSleep(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"fail-before-sleep",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "", errors.New("step exploded")
			})
			if err != nil {
				return nil, err
			}

			// Should never reach here
			if err := workflow.Sleep(wctx, 1*time.Hour); err != nil {
				return nil, err
			}

			return &SleepTestOutput{Result: "unreachable"}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "step exploded")
	require.NotErrorIs(t, err, workflow.ErrWorkflowSleeping)
}

func TestSleep_VeryLongDuration_90Days(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	runner := setupSleepRunner(t, clock)

	wf := workflow.New(
		runner,
		"sleep-90-days",
		func(wctx *workflow.Context, params *SleepTestParams) (*SleepTestOutput, error) {
			_, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "before", nil
			})
			if err != nil {
				return nil, err
			}

			// 90 day sleep
			if err := workflow.Sleep(wctx, 90*24*time.Hour); err != nil {
				return nil, err
			}

			result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
				return "after-90-days", nil
			})
			if err != nil {
				return nil, err
			}

			return &SleepTestOutput{Result: result}, nil
		},
	)

	ctx := t.Context()
	instanceID, err := wf.Start(ctx, &SleepTestParams{})
	require.NoError(t, err)

	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	// Simulate checking every 30 days
	clock.Advance(30 * 24 * time.Hour)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	clock.Advance(30 * 24 * time.Hour)
	_, err = wf.Run(ctx, instanceID)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)

	clock.Advance(31 * 24 * time.Hour) // 91 days total
	output, err := wf.Run(ctx, instanceID)
	require.NoError(t, err)
	require.Equal(t, "after-90-days", output.Result)
}
