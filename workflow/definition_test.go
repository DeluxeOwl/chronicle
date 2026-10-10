package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_CannotRunAnInstanceUsingAnotherDefinition(t *testing.T) {
	log := eventlog.NewMemory()
	runner, err := workflow.NewRunner(log, nil)
	require.NoError(t, err)
	original := workflow.New(
		runner,
		"original",
		func(*workflow.Context, *struct{}) (*WorkerTestOutput, error) {
			return &WorkerTestOutput{Result: "original"}, nil
		},
	)
	other := workflow.New(
		runner,
		"other",
		func(*workflow.Context, *struct{}) (*WorkerTestOutput, error) {
			t.Error("wrong workflow callback executed")
			return nil, errors.New("wrong workflow callback")
		},
	)
	id, err := original.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = other.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure)
	out, err := original.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "original", out.Result)
}

func TestWorkflow_RecordedStepMetadataRejectsIncompatibleReplay(t *testing.T) {
	log := eventlog.NewMemory()
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"stable",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			if err := workflow.Step2(wctx, func(context.Context) error { return nil }, workflow.WithStepName("Prepare")); err != nil {
				return nil, err
			}
			if err := workflow.Sleep(wctx, time.Hour); err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "complete"}, nil
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	clock.Advance(time.Hour)
	other, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	changed := workflow.New(
		other,
		"stable",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			if err := workflow.Step2(wctx, func(context.Context) error { t.Error("incompatible callback ran"); return nil }, workflow.WithStepName("Different")); err != nil {
				return nil, err
			}
			return nil, errors.New("should not reach here")
		},
	)
	_, err = changed.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure)
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, info.Status)
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "complete", out.Result)
}

func TestWorkflow_LatePublicationDoesNotResolveExpiredWait(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		setupTestDB(t),
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"late-publication",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			_, err := workflow.WaitForEvent[string](
				wctx,
				"shipment:late",
				workflow.WaitForEventOptions{Timeout: time.Hour},
			)
			return nil, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	clock.Advance(time.Hour)
	require.NoError(t, workflow.PublishEvent(t.Context(), runner, "shipment:late", "too-late"))
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrEventTimeout)
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusFailed, info.Status)
}
