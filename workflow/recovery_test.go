package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_WaitUsesPublicationTimeRatherThanWorkerResumeTime(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "sqlite"}[durable], func(t *testing.T) {
			clock := newClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
			var runner *workflow.Runner
			var err error
			if durable {
				runner, err = workflow.NewSqliteRunnerWithSyncQueue(
					t.Context(),
					setupTestDB(t),
					workflow.WithNowFunc(clock.Now),
				)
			} else {
				runner, err = workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithNowFunc(clock.Now))
			}
			require.NoError(t, err)
			wf := workflow.New(
				runner,
				"on-time-event",
				func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
					payload, err := workflow.WaitForEvent[string](
						wctx,
						"delivery:42",
						workflow.WaitForEventOptions{Timeout: time.Hour},
					)
					if err != nil {
						return nil, err
					}
					return &WorkerTestOutput{Result: payload}, nil
				},
			)
			id, err := wf.Start(t.Context(), &struct{}{})
			require.NoError(t, err)
			_, err = wf.Run(t.Context(), id)
			require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
			clock.Advance(30 * time.Minute)
			require.NoError(t, workflow.PublishEvent(t.Context(), runner, "delivery:42", "on-time"))
			clock.Advance(time.Hour)
			out, err := wf.Run(t.Context(), id)
			require.NoError(t, err, "worker scheduling delay must not discard an on-time event")
			require.Equal(t, "on-time", out.Result)
		})
	}
}

func TestWorkflow_StepFailureIsNotTerminalBeforeTheRetryDecision(t *testing.T) {
	log := eventlog.NewMemory()
	failure := errors.New("retry checkpoint unavailable")
	broken := &failRetryLog{Log: log, err: failure}
	runner, err := workflow.NewRunner(broken, nil)
	require.NoError(t, err)
	attempts := 0
	register := func(r *workflow.Runner) *workflow.Workflow[struct{}, WorkerTestOutput] {
		return workflow.New(
			r,
			"recover",
			func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
				value, err := workflow.Step(wctx, func(context.Context) (string, error) {
					attempts++
					if attempts == 1 {
						return "", errors.New("temporary domain failure")
					}
					return "recovered", nil
				})
				if err != nil {
					return nil, err
				}
				return &WorkerTestOutput{Result: value}, nil
			},
		)
	}
	wf := register(runner)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(workflow.DefaultRetryStrategy()),
	)
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, failure)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Len(
		t,
		detail.History,
		1,
		"a failed retry save must not leave a partial failure decision",
	)
	other, err := workflow.NewRunner(log, nil)
	require.NoError(t, err)
	resumed := register(other)
	out, err := resumed.Run(t.Context(), id)
	require.NoError(t, err, "a step-failure checkpoint must not permanently strand the workflow")
	require.Equal(t, "recovered", out.Result)
}

type failRetryLog struct {
	event.Log
	err error
}

func (l *failRetryLog) AppendEvents(
	ctx context.Context,
	id event.LogID,
	check version.Check,
	events event.RawEvents,
) (version.Version, error) {
	for _, evt := range events {
		if evt.EventName() == "workflow/retried" {
			return version.Zero, l.err
		}
	}
	return l.Log.AppendEvents(ctx, id, check, events)
}

func TestWorkflow_DirectRunDoesNotRepeatCodeBeforeAnUnfinishedSleep(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	entries := 0
	wf := workflow.New(
		runner,
		"sleep-guard",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			entries++
			if err := workflow.Sleep(wctx, time.Hour, workflow.WithStepName("Cooling period")); err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "awake"}, nil
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	require.Equal(t, 1, entries)
	clock.Advance(time.Hour)
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "awake", out.Result)
}

func TestWorkflow_WorkerShutdownDoesNotFailTheWorkflow(t *testing.T) {
	log := eventlog.NewMemory()
	runner, err := workflow.NewRunner(log, nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	wf := workflow.New(
		runner,
		"interruptible",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(context.Context) (string, error) {
				cancel()
				return "", context.Canceled
			})
			return nil, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(ctx, id)
	require.ErrorIs(t, err, context.Canceled)
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(
		t,
		workflow.StatusRunning,
		info.Status,
		"stopping a worker is not a domain failure",
	)
}

func TestWorkflow_CheckpointFailureDoesNotCreateADomainFailure(t *testing.T) {
	log := eventlog.NewMemory()
	failure := errors.New("checkpoint unavailable")
	broken := &failCheckpointLog{Log: log, err: failure}
	runner, err := workflow.NewRunner(broken, nil)
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"checkpoint",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			value, err := workflow.Step(
				wctx,
				func(context.Context) (string, error) { return "ok", nil },
			)
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: value}, nil
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, failure)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, detail.Summary.Status)
	for _, e := range detail.History {
		require.NotEqual(t, "workflow/failed", e.EventName)
	}
}

type failCheckpointLog struct {
	event.Log
	err error
}

func (l *failCheckpointLog) AppendEvents(
	ctx context.Context,
	id event.LogID,
	check version.Check,
	events event.RawEvents,
) (version.Version, error) {
	for _, evt := range events {
		if evt.EventName() == "workflow/step_completed" {
			return version.Zero, l.err
		}
	}
	return l.Log.AppendEvents(ctx, id, check, events)
}

func TestWorkflow_ZeroMaxAttemptsUsesFiveAttempts(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	attempts := 0
	wf := workflow.New(
		runner,
		"default-attempt-limit",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			_, err := workflow.Step(
				wctx,
				func(context.Context) (string, error) { attempts++; return "", errors.New("domain error") },
			)
			return nil, err
		},
	)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(
			workflow.RetryStrategy{
				MaxAttempts: 0,
				BaseDelay:   time.Second,
				Factor:      1,
				MaxDelay:    time.Minute,
			},
		),
	)
	require.NoError(t, err)
	for range 4 {
		_, err = wf.Run(t.Context(), id)
		require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
		clock.Advance(time.Second)
	}
	_, err = wf.Run(t.Context(), id)
	require.Error(t, err)
	require.NotErrorIs(t, err, workflow.ErrWorkflowRetrying)
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusFailed, info.Status)
	require.Equal(t, 5, attempts)
}

func TestWorkflow_DirectRunHonorsThePersistedRetryDeadline(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	attempts := 0
	wf := workflow.New(
		runner,
		"retry",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			_, err := workflow.Step(
				wctx,
				func(context.Context) (string, error) { attempts++; return "", errors.New("retry me") },
			)
			return nil, err
		},
	)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(
			workflow.RetryStrategy{
				MaxAttempts: 3,
				BaseDelay:   time.Hour,
				Factor:      1,
				MaxDelay:    time.Hour,
			},
		),
	)
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	require.Equal(t, 1, attempts, "direct Run must not bypass persisted backoff")
}
