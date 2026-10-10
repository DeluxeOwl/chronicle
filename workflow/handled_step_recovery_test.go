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

type recoveryOutput struct {
	Value string `json:"value"`
}

func TestWorkflow_HandledStepFailureSurvivesRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db := setupSyncDB(t)
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	primaryCalls, fallbackCalls := 0, 0
	definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
		value, err := workflow.Step(wctx, func(context.Context) (string, error) {
			primaryCalls++
			if primaryCalls == 1 {
				return "", errors.New("primary service temporarily unavailable")
			}
			return "primary", nil
		}, workflow.WithStepName("Primary"))
		if err != nil {
			if errors.Is(err, workflow.ErrWorkflowInfrastructure) ||
				errors.Is(err, workflow.ErrConflictAbort) {
				return nil, err
			}
			value, err = workflow.Step(wctx, func(context.Context) (string, error) {
				fallbackCalls++
				return "fallback", nil
			}, workflow.WithStepName("Fallback"))
			if err != nil {
				return nil, err
			}
		}
		if err := workflow.Sleep(wctx, time.Hour, workflow.WithStepName("Cooling period")); err != nil {
			return nil, err
		}
		return &recoveryOutput{Value: value}, nil
	}
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(ctx, db, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	original := workflow.New(runner, "handled-failure", definition)
	id, err := original.Start(ctx, &struct{}{})
	require.NoError(t, err)
	_, err = original.Run(ctx, id)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	require.Equal(t, 1, primaryCalls)

	clock.Advance(time.Hour)
	restarted, err := workflow.NewSqliteRunnerWithSyncQueue(
		ctx,
		db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	resumed := workflow.New(restarted, "handled-failure", definition)
	output, runErr := resumed.Run(ctx, id)
	info, err := resumed.GetStatus(ctx, id)
	require.NoError(t, err)
	t.Logf(
		"unchanged definition after restart: output=%+v err=%v primaryCalls=%d persisted=%+v",
		output,
		runErr,
		primaryCalls,
		info,
	)
	require.NoError(
		t,
		runErr,
		"a handled failure must not change the already checkpointed fallback path",
	)
	require.Equal(t, "fallback", output.Value)
	require.Equal(
		t,
		1,
		primaryCalls,
		"the handled outcome must replay rather than run another primary effect",
	)
	require.Equal(t, 1, fallbackCalls, "the selected fallback checkpoint must replay too")
}

type streamReadFailureLog struct {
	event.Log
	failingID event.LogID
	failure   error
}

func (l *streamReadFailureLog) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	if id != l.failingID {
		return l.Log.ReadEvents(ctx, id, selector)
	}
	return func(yield func(*event.Record, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		yield(nil, l.failure)
	}
}

func TestWorkflow_CancelStorageFailureIsNotADomainFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	storageErr := errors.New("temporary event-log read outage")
	log := &streamReadFailureLog{Log: eventlog.NewMemory(), failingID: "", failure: storageErr}
	runner, err := workflow.NewRunner(log, nil)
	require.NoError(t, err)
	target := workflow.New(
		runner,
		"cancel-target",
		func(*workflow.Context, *struct{}) (*recoveryOutput, error) {
			return &recoveryOutput{Value: "unused"}, nil
		},
	)
	targetID, err := target.Start(ctx, &struct{}{})
	require.NoError(t, err)
	log.failingID = event.LogID(targetID)
	parent := workflow.New(
		runner,
		"cancel-previous",
		func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
			if err := workflow.Step2(wctx, func(stepCtx context.Context) error {
				return workflow.CancelWorkflow(stepCtx, runner, targetID, "superseded")
			}, workflow.WithStepName("Cancel previous")); err != nil {
				return nil, err
			}
			return &recoveryOutput{Value: "done"}, nil
		},
	)
	id, err := parent.Start(ctx, &struct{}{})
	require.NoError(t, err)
	_, runErr := parent.Run(ctx, id)
	require.ErrorIs(t, runErr, storageErr)
	log.failingID = ""
	info, err := parent.GetStatus(ctx, id)
	require.NoError(t, err)
	_, recoveryErr := parent.Run(ctx, id)
	t.Logf(
		"cancel read outage: err=%v persisted=%+v after storage recovers=%v",
		runErr,
		info,
		recoveryErr,
	)
	require.ErrorIs(
		t,
		runErr,
		workflow.ErrWorkflowInfrastructure,
		"workflow helpers must preserve infrastructure classification",
	)
	require.Equal(t, workflow.StatusRunning, info.Status)
	require.NoError(t, recoveryErr)
}
