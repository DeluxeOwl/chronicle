package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestInspector_ProjectsDeferredStepsAndDoesNotInferSleepFromUserResults(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	log := eventlog.NewMemory()
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"deferred",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			if err := workflow.Step2(wctx, func(context.Context) error { return nil }, workflow.WithStepName("Prepare")); err != nil {
				return nil, err
			}
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
	inspector, err := workflow.NewInspector(log, workflow.WithInspectorNowFunc(clock.Now))
	require.NoError(t, err)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusSleeping, detail.Summary.Status)
	require.Equal(t, "Cooling period", detail.Steps[1].Name)
	require.Equal(t, "sleeping", detail.Steps[1].Status)
	require.Nil(
		t,
		detail.Steps[1].CompletedAt,
		"persisting a sleep deadline is not completing the sleep",
	)
	require.Equal(t, 1, detail.Summary.CompletedSteps)
	clock.Advance(time.Hour)
	_, err = wf.Run(t.Context(), id)
	require.NoError(t, err)
	detail, err = inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCompleted, detail.Summary.Status)
	require.NotNil(t, detail.Steps[1].CompletedAt)

	other := workflow.New(
		runner,
		"normal-result",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			_, err := workflow.Step(wctx, func(context.Context) (map[string]string, error) {
				return map[string]string{"wakeAt": "2099-01-01T00:00:00Z"}, nil
			})
			if err != nil {
				return nil, err
			}
			_, err = workflow.WaitForEvent[string](wctx, "never-arrives")
			return nil, err
		},
	)
	otherID, err := other.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = other.Run(t.Context(), otherID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	otherDetail, err := inspector.Get(t.Context(), otherID)
	require.NoError(t, err)
	require.Equal(t, "step", otherDetail.Steps[0].Kind)
	require.Nil(t, otherDetail.Steps[0].WakeAt)
	require.Equal(t, "waiting", otherDetail.Steps[1].Status)
}

func TestInspector_ListsAndReadsPersistedWorkflows(t *testing.T) {
	log := eventlog.NewMemory()
	clock := newClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"welcome-email",
		func(wctx *workflow.Context, params *WorkerTestParams) (*WorkerTestOutput, error) {
			value, err := workflow.Step(
				wctx,
				func(context.Context) (string, error) { return "hello-" + params.Value, nil },
			)
			if err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: value}, nil
		},
	)
	id, err := wf.Start(t.Context(), &WorkerTestParams{Value: "Alice"})
	require.NoError(t, err)
	clock.Advance(time.Minute)
	_, err = wf.Run(t.Context(), id)
	require.NoError(t, err)

	inspector, err := workflow.NewInspector(log, workflow.WithInspectorNowFunc(clock.Now))
	require.NoError(t, err)
	page, err := inspector.List(t.Context(), workflow.ListOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, page.Total)
	require.Len(t, page.Instances, 1)
	require.Equal(t, "welcome-email", page.Instances[0].WorkflowName)
	require.Equal(t, workflow.StatusCompleted, page.Instances[0].Status)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, id, detail.Summary.InstanceID)
	require.JSONEq(t, `{"value":"Alice"}`, string(detail.Params))
	require.JSONEq(t, `{"result":"hello-Alice"}`, string(detail.Output))
	require.Len(t, detail.Steps, 1)
	require.Equal(t, "completed", detail.Steps[0].Status)
	require.JSONEq(t, `"hello-Alice"`, string(detail.Steps[0].Result))
	require.GreaterOrEqual(t, len(detail.History), 3)
}
