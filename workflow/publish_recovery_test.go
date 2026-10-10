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

func TestWorkflow_PublicationStorageFailureInsideAStepRemainsRecoverable(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "without-domain-retry", true: "with-domain-retry"}[retry],
			func(t *testing.T) {
				db := setupTestDB(t)
				runner, err := workflow.NewSqliteRunnerWithSyncQueue(t.Context(), db)
				require.NoError(t, err)
				wf := workflow.New(
					runner,
					"publication-step",
					func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
						if err := workflow.Step2(wctx, func(ctx context.Context) error { return workflow.PublishEvent(ctx, runner, "inside:step", "approved") }); err != nil {
							return nil, err
						}
						return &WorkerTestOutput{Result: "published"}, nil
					},
				)
				var opts []workflow.StartOption
				if retry {
					opts = []workflow.StartOption{
						workflow.WithRetryStrategy(workflow.DefaultRetryStrategy()),
					}
				}
				id, err := wf.Start(t.Context(), &struct{}{}, opts...)
				require.NoError(t, err)
				_, err = db.ExecContext(
					t.Context(),
					`CREATE TRIGGER reject_publication BEFORE INSERT ON workflow_published_events BEGIN SELECT RAISE(ABORT,'injected publication failure'); END`,
				)
				require.NoError(t, err)
				_, err = wf.Run(t.Context(), id)
				require.ErrorIs(
					t,
					err,
					workflow.ErrWorkflowInfrastructure,
					"publication persistence is not a domain failure",
				)
				info, err := wf.GetStatus(t.Context(), id)
				require.NoError(t, err)
				require.Equal(t, workflow.StatusRunning, info.Status)
				require.Equal(
					t,
					1,
					info.Attempt,
					"storage failures must not consume domain retries",
				)
				_, err = db.ExecContext(t.Context(), `DROP TRIGGER reject_publication`)
				require.NoError(t, err)
				out, err := wf.Run(t.Context(), id)
				require.NoError(t, err)
				require.Equal(t, "published", out.Result)
			},
		)
	}
}

func TestWorkflow_PublicationEncodingFailureDoesNotConsumeDomainRetries(t *testing.T) {
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(t.Context(), setupTestDB(t))
	require.NoError(t, err)
	invalid := true
	wf := workflow.New(
		runner,
		"publication-encoding",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			if err := workflow.Step2(wctx, func(ctx context.Context) error {
				var payload any = "encoded"
				if invalid {
					payload = make(chan string)
				}
				return workflow.PublishEvent(ctx, runner, "encoded:publication", payload)
			}); err != nil {
				return nil, err
			}
			return &WorkerTestOutput{Result: "published"}, nil
		},
	)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(workflow.DefaultRetryStrategy()),
	)
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure)
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, info.Status)
	require.Equal(t, 1, info.Attempt)
	invalid = false
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "published", out.Result)
}

func TestWorkflow_EventPublicationAndWakeupRollBackTogether(t *testing.T) {
	db := setupTestDB(t)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(t.Context(), db)
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"atomic-publication",
		func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
			payload, err := workflow.WaitForEvent[string](wctx, "approval:42")
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
	// A DB trigger injects a real failure at the wake-up write boundary.
	_, err = db.ExecContext(
		t.Context(),
		`CREATE TRIGGER reject_wakeup BEFORE INSERT ON workflow_ready_tasks BEGIN SELECT RAISE(ABORT,'injected wakeup failure'); END`,
	)
	require.NoError(t, err)
	require.Error(t, workflow.PublishEvent(t.Context(), runner, "approval:42", "uncommitted"))
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER reject_wakeup`)
	require.NoError(t, err)
	require.NoError(t, workflow.PublishEvent(t.Context(), runner, "approval:42", "committed"))
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(
		t,
		"committed",
		out.Result,
		"the failed transaction must not consume the first publication",
	)
}

func TestWorkflow_PublishedEventWakesWorkerAfterDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflows.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
		require.NoError(t, err)
		db.SetMaxOpenConns(1)
		return db
	}
	register := func(runner *workflow.Runner) *workflow.Workflow[struct{}, WorkerTestOutput] {
		return workflow.New(
			runner,
			"durable-publication",
			func(wctx *workflow.Context, _ *struct{}) (*WorkerTestOutput, error) {
				payload, err := workflow.WaitForEvent[string](wctx, "approval:restart")
				if err != nil {
					return nil, err
				}
				return &WorkerTestOutput{Result: payload}, nil
			},
		)
	}
	db := open()
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(t.Context(), db)
	require.NoError(t, err)
	wf := register(runner)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	require.NoError(t, workflow.PublishEvent(t.Context(), runner, "approval:restart", "approved"))
	require.NoError(t, db.Close())
	db = open()
	t.Cleanup(func() { _ = db.Close() })
	runner, err = workflow.NewSqliteRunnerWithSyncQueue(t.Context(), db)
	require.NoError(t, err)
	wf = register(runner)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: 0})
	}()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(
		t,
		func() bool { out, err := wf.GetResult(t.Context(), id); return err == nil && out.Result == "approved" },
		time.Second,
		time.Millisecond,
	)
}
