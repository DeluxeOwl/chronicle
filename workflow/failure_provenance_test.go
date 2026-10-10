package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_RetryClearsTheRethrownFailureAfterCleanupCheckpoint(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	log := eventlog.NewMemory()
	calls, cleanupCalls := 0, 0
	definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
		result, primaryErr := workflow.Step(wctx, func(context.Context) (string, error) {
			calls++
			if calls == 1 {
				return "", errors.New("primary operation failed")
			}
			return "recovered", nil
		}, workflow.WithStepName("Primary"))
		// Run cleanup in both branches so the checkpoint path stays compatible.
		if err := workflow.Step2(wctx, func(context.Context) error {
			cleanupCalls++
			return nil
		}, workflow.WithStepName("Cleanup")); err != nil {
			return nil, err
		}
		if primaryErr != nil {
			return nil, fmt.Errorf("after cleanup: %w", primaryErr)
		}
		return &recoveryOutput{Value: result}, nil
	}
	runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	wf := workflow.New(runner, "rethrow-after-cleanup", definition)
	id, err := wf.Start(t.Context(), &struct{}{}, workflow.WithRetryStrategy(workflow.RetryStrategy{
		MaxAttempts: 2, BaseDelay: time.Minute, Factor: 1, MaxDelay: time.Minute,
	}))
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	clock.Advance(time.Minute)
	restarted, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
	require.NoError(t, err)
	resumed := workflow.New(restarted, "rethrow-after-cleanup", definition)
	out, err := resumed.Run(t.Context(), id)
	require.NoError(t, err, "the propagated failure, not cleanup success, identifies what to retry")
	require.Equal(t, "recovered", out.Value)
	require.Equal(t, 2, calls)
	require.Equal(t, 1, cleanupCalls, "already checkpointed cleanup must replay")
}
