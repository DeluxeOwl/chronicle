package monitor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestMonitor_UnnamedStepFailureDoesNotInventSleepCompletionTime(t *testing.T) {
	log := eventlog.NewMemory()
	clock := &sleepHistoryClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	runner := newRunner(t, log, workflow.WithNowFunc(clock.Now))
	failure := errors.New("activity rejected after another thirty seconds")
	wf := workflow.New(
		runner,
		"sleep-then-unnamed-failure",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			if err := workflow.Sleep(wctx, time.Minute); err != nil {
				return nil, err
			}
			err := workflow.Step2(wctx, func(context.Context) error {
				clock.now = clock.now.Add(30 * time.Second)
				return failure
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowSleeping)
	clock.now = clock.now.Add(time.Minute)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, failure)
	inspector, err := workflow.NewInspector(log, workflow.WithInspectorNowFunc(clock.Now))
	require.NoError(t, err)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "completed", detail.Steps[0].Status)
	require.Nil(
		t,
		detail.Steps[0].CompletedAt,
		"the activity failure time is not the sleep resumption time",
	)
}
