package workflow_test

import (
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_ClockZoneDoesNotMovePersistedSleepDeadline(t *testing.T) {
	clock := newClock(
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(time.FixedZone("second-offset", -90)),
	)
	log := eventlog.NewMemory()
	definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
		if err := workflow.Sleep(wctx, time.Minute, workflow.WithStepName("Delay")); err != nil {
			return nil, err
		}
		return &recoveryOutput{Value: "after-deadline"}, nil
	}
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(runner, "second-zone-sleep", definition)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	clock.Advance(30 * time.Second)
	restarted, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	resumed := workflow.New(restarted, "second-zone-sleep", definition)
	_, err = resumed.Run(t.Context(), id)
	require.ErrorIs(
		t,
		err,
		workflow.ErrWorkflowSleeping,
		"persisted instant must remain later than the half-minute clock",
	)
	clock.Advance(30 * time.Second)
	out, err := resumed.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "after-deadline", out.Value)
}
