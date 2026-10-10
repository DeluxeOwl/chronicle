package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestWorkflow_ExpiredWorkerCannotCancelItsOwnInstance(t *testing.T) {
	clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		setupSyncDB(t),
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	commandResult := make(chan error, 1)
	wf := workflow.New(
		runner,
		"expired-self-cancel",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				clock.Advance(time.Minute)
				commandResult <- workflow.CancelWorkflow(ctx, runner, wctx.InstanceID(), "stale request")
				<-ctx.Done()
				return ctx.Err()
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	var cancelErr error
	select {
	case cancelErr = <-commandResult:
	case <-ctx.Done():
		t.Fatal("the worker did not issue its cancellation command")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the worker did not stop")
	}
	require.ErrorIs(
		t,
		cancelErr,
		workflow.ErrStaleClaim,
		"self-targeted commands must retain checkpoint receipt fencing",
	)
	status, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, status.Status)
}
