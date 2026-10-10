package workflow_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func commandContextRunner(t *testing.T) *workflow.Runner {
	t.Helper()
	db, err := sql.Open(
		"sqlite3",
		filepath.Join(t.TempDir(), "commands.db")+"?_journal_mode=WAL&_busy_timeout=5000",
	)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(t.Context(), db)
	require.NoError(t, err)
	return runner
}

func TestWorkflow_WorkerCanCancelAnotherInstance(t *testing.T) {
	runner := commandContextRunner(t)
	target := workflow.New(
		runner,
		"worker-cancel-target",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			_, err := workflow.WaitForEvent[string](wctx, "never-published")
			return &struct{}{}, err
		},
	)
	targetID, err := target.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = target.Run(t.Context(), targetID)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	parent := workflow.New(
		runner,
		"worker-cancel-parent",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				return workflow.CancelWorkflow(ctx, runner, targetID, "superseded")
			})
			return &struct{}{}, err
		},
	)
	id, err := parent.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(
		t,
		runner.RunWorker(
			ctx,
			workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: 0},
		),
		context.DeadlineExceeded,
	)
	_, err = parent.GetResult(t.Context(), id)
	require.NoError(
		t,
		err,
		"the invoking worker receipt must not be mistaken for the target instance receipt",
	)
	status, err := target.GetStatus(t.Context(), targetID)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCancelled, status.Status)
}

func TestWorkflow_WorkerCanStartAnotherInstance(t *testing.T) {
	runner := commandContextRunner(t)
	child := workflow.New(
		runner,
		"worker-start-child",
		func(*workflow.Context, *struct{}) (*struct{}, error) {
			return &struct{}{}, nil
		},
	)
	parent := workflow.New(
		runner,
		"worker-start-parent",
		func(wctx *workflow.Context, _ *struct{}) (*workflow.InstanceID, error) {
			id, err := workflow.Step(wctx, func(ctx context.Context) (workflow.InstanceID, error) {
				return child.Start(ctx, &struct{}{})
			})
			return &id, err
		},
	)
	id, err := parent.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(
		t,
		runner.RunWorker(
			ctx,
			workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: 0},
		),
		context.DeadlineExceeded,
	)
	childID, err := parent.GetResult(t.Context(), id)
	require.NoError(t, err, "an active worker can issue a start command for a different stream")
	_, err = child.GetResult(t.Context(), *childID)
	require.NoError(t, err)
}
