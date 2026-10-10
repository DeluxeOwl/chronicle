//go:build !windows

package workflow_test

import (
	"context"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestDistributedWorkflow_PostgresCommittedCheckpointLosesResponse(t *testing.T) {
	database, _ := startDistributedPostgres(t)
	healthyDB := openDistributedDB(t, distributedPostgres, database)
	proxied, proxy := startDistributedBlackholeProxy(
		t,
		database,
		"chronicle-unknown-checkpoint-commit",
	)
	faultyDB := openDistributedDB(t, distributedPostgres, proxied)
	healthy, err := newDistributedRunner(t.Context(), healthyDB, distributedPostgres)
	require.NoError(t, err)
	faulty, err := newDistributedRunner(t.Context(), faultyDB, distributedPostgres,
		workflow.WithPostgresQueueOpts(workflow.WithPostgresQueueTransactionTimeout(2*time.Second)),
	)
	require.NoError(t, err)
	var effects atomic.Int32
	definition := func(wctx *workflow.Context, _ *struct{}) (*string, error) {
		result, err := workflow.Step(wctx, func(context.Context) (string, error) {
			effects.Add(1)
			proxy.armCommit()
			return "committed", nil
		})
		return &result, err
	}
	original := workflow.New(faulty, "ambiguous-checkpoint", definition)
	recovery := workflow.New(healthy, "ambiguous-checkpoint", definition)
	id, err := recovery.Start(t.Context(), &struct{}{})
	require.NoError(t, err)

	_, err = original.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure,
		"a withheld COMMIT response must not be recorded as a domain failure")
	distributedBlackholeAwait(t, t.Context(), proxy.trapped, "COMMIT completion was not withheld")
	require.Equal(t, int32(1), effects.Load())
	status, err := recovery.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, status.Status)
	require.Equal(t, 1, status.Attempt)

	out, err := recovery.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "committed", *out)
	require.Equal(
		t,
		int32(1),
		effects.Load(),
		"replay must discover the committed checkpoint instead of repeating its effect",
	)
	status, err = recovery.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCompleted, status.Status)
}

func TestDistributedWorkflow_PostgresCommittedPublicationLosesResponse(t *testing.T) {
	database, _ := startDistributedPostgres(t)
	db, wf, inspector := newDistributedFixture(t, distributedPostgres, database)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWait, Sleep: 0})
	require.NoError(t, err)
	waiting := startDistributedProcess(
		t,
		childWorker(distributedPostgres, database, "before-unknown-commit", false, true),
	)
	waiting.waitMessage(t, "ready")
	require.Eventually(t, func() bool {
		detail, err := inspector.Get(t.Context(), id)
		return err == nil && detail.Summary.Status == workflow.StatusWaiting
	}, 8*time.Second, 10*time.Millisecond)
	waiting.kill()

	proxied, proxy := startDistributedBlackholeProxy(
		t,
		database,
		"chronicle-unknown-publication-commit",
	)
	faultyDB := openDistributedDB(t, distributedPostgres, proxied)
	publisher, err := newDistributedRunner(t.Context(), faultyDB, distributedPostgres,
		workflow.WithPostgresQueueOpts(workflow.WithPostgresQueueTransactionTimeout(2*time.Second)),
	)
	require.NoError(t, err)
	proxy.armCommit()
	err = workflow.PublishEvent(
		t.Context(),
		publisher,
		distributedEventName,
		"committed-before-response-loss",
	)
	require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure)
	distributedBlackholeAwait(
		t,
		t.Context(),
		proxy.trapped,
		"publication COMMIT completion was not withheld",
	)
	healthyPublisher, err := newDistributedRunner(t.Context(), db, distributedPostgres)
	require.NoError(t, err)
	require.NoError(
		t,
		workflow.PublishEvent(
			t.Context(),
			healthyPublisher,
			distributedEventName,
			"must-not-replace-first",
		),
	)

	replacement := startDistributedProcess(
		t,
		childWorker(distributedPostgres, database, "after-unknown-commit", false, true),
	)
	replacement.waitMessage(t, "ready")
	out := distributedResult(t, wf, id)
	require.Equal(t, "committed-before-response-loss", out.Approval)
	require.Equal(t, "after-unknown-commit", out.CompletedBy)
	require.Equal(t, 1, distributedCalls(t, db, distributedPostgres, id, "prepare"))
	require.Equal(t, 1, distributedCalls(t, db, distributedPostgres, id, "finish"))
}

func TestDistributedWorkflow_PostgresServerRestartRecoversCheckpoint(t *testing.T) {
	database, container := startDistributedPostgres(t)
	db, wf, _ := newDistributedFixture(t, distributedPostgres, database)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
	require.NoError(t, err)
	original := startDistributedProcess(
		t,
		childWorker(distributedPostgres, database, "before-server-restart", true, true),
	)
	original.waitMessage(t, "ready")
	original.waitMessage(t, "entered")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	immediate := time.Duration(0)
	require.NoError(t, container.Stop(ctx, &immediate))
	original.kill()
	require.Error(
		t,
		db.PingContext(ctx),
		"the database must actually be unavailable during the injected restart",
	)
	require.NoError(t, container.Start(ctx))
	// Docker may assign a new ephemeral host port when the container restarts.
	// Reopen the same persisted database at its current endpoint.
	databaseURL, err := url.Parse(database)
	require.NoError(t, err)
	databaseURL.Host, err = container.Endpoint(ctx, "")
	require.NoError(t, err)
	database = databaseURL.String()
	db = openDistributedDB(t, distributedPostgres, database)
	require.NoError(t, db.PingContext(ctx))
	runner, err := newDistributedRunner(ctx, db, distributedPostgres)
	require.NoError(t, err)
	wf = workflow.New(
		runner,
		"distributed-workflow",
		func(*workflow.Context, *distributedParams) (*distributedOutput, error) {
			panic("the parent only reads persisted results")
		},
	)
	log, err := eventlog.NewPostgres(db)
	require.NoError(t, err)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)

	replacement := startDistributedProcess(
		t,
		childWorker(distributedPostgres, database, "after-server-restart", false, true),
	)
	replacement.waitMessage(t, "ready")
	out := distributedResult(t, wf, id)
	require.Equal(t, "before-server-restart", out.PreparedBy)
	require.Equal(t, "after-server-restart", out.CompletedBy)
	require.Equal(t, 1, distributedCalls(t, db, distributedPostgres, id, "prepare"))
	require.Equal(t, 2, distributedCalls(t, db, distributedPostgres, id, "finish"))
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, 1, detail.Summary.Attempt)
	for _, record := range detail.History {
		require.NotEqual(t, "workflow/failed", record.EventName)
	}
}
