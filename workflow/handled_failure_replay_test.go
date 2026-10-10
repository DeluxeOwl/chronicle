package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_HandledFailureRemainsStableAcrossLaterWorkflowRetry(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	db := setupSyncDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	primaryCalls, workflowCalls := 0, 0
	definition := func(wctx *workflow.Context, _ *struct{}) (*recoveryOutput, error) {
		_, err := workflow.Step(wctx, func(context.Context) (string, error) {
			primaryCalls++
			return "", errors.New("use fallback")
		}, workflow.WithStepName("Primary"))
		if err == nil || errors.Is(err, workflow.ErrWorkflowInfrastructure) ||
			errors.Is(err, workflow.ErrConflictAbort) {
			return nil, err
		}
		fallback, err := workflow.Step(wctx, func(context.Context) (string, error) {
			return "fallback", nil
		}, workflow.WithStepName("Fallback"))
		if err != nil {
			return nil, err
		}
		workflowCalls++
		if workflowCalls == 1 {
			return nil, errors.New("later workflow decision requests retry")
		}
		return &recoveryOutput{Value: fallback}, nil
	}
	original := workflow.New(runner, "handled-before-retry", definition)
	id, err := original.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(workflow.RetryStrategy{
			MaxAttempts: 2, BaseDelay: time.Minute, Factor: 1, MaxDelay: time.Minute,
		}),
	)
	require.NoError(t, err)
	_, err = original.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowRetrying)
	clock.Advance(time.Minute)
	restarted, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	resumed := workflow.New(restarted, "handled-before-retry", definition)
	out, err := resumed.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "fallback", out.Value)
	require.Equal(
		t,
		1,
		primaryCalls,
		"retrying an unrelated later error must not erase an already handled outcome",
	)
	info, err := resumed.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 2, info.Attempt)
}
