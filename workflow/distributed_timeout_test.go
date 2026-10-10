//go:build !windows

package workflow_test

import (
	"context"
	"database/sql"
	"net/url"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func distributedPostgresTimeoutCheckpointObservesPendingPublication(
	t *testing.T,
	backend, database string,
) {
	if backend != distributedPostgres {
		t.Skip("SQLite has a fake-clock transaction-order regression")
	}
	db := openDistributedDB(t, backend, database)
	clock := newClock(time.Now())
	deadline := clock.Now().Add(time.Hour)
	runner, err := workflow.NewPostgresRunnerWithSyncQueue(
		t.Context(),
		db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"postgres-timeout-order",
		func(wctx *workflow.Context, _ *struct{}) (*string, error) {
			payload, err := workflow.WaitForEvent[string](
				wctx,
				"approval:ordered",
				workflow.WaitForEventOptions{Timeout: time.Hour},
			)
			if err != nil {
				return nil, err
			}
			return &payload, nil
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	log, err := eventlog.NewPostgres(db)
	require.NoError(t, err)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)
	before, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	publisherDB := postgresApplicationDB(t, database, "chronicle-pending-publication")
	publisher, err := workflow.NewPostgresRunnerWithSyncQueue(t.Context(), publisherDB)
	require.NoError(t, err)
	resumeDB := postgresApplicationDB(t, database, "chronicle-timeout-resume")
	resumeRunner, err := workflow.NewPostgresRunnerWithSyncQueue(
		t.Context(),
		resumeDB,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	resumed := workflow.New(
		resumeRunner,
		"postgres-timeout-order",
		func(wctx *workflow.Context, _ *struct{}) (*string, error) {
			payload, err := workflow.WaitForEvent[string](
				wctx,
				"approval:ordered",
				workflow.WaitForEventOptions{Timeout: time.Hour},
			)
			if err != nil {
				return nil, err
			}
			return &payload, nil
		},
	)
	// Stop publication after it stores accepted time and before task wake-up
	// can finish. The payload is not yet visible to other transactions.
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var locked string
	require.NoError(
		t,
		tx.QueryRowContext(t.Context(), "SELECT instance_id FROM workflow_ready_tasks WHERE instance_id=$1 FOR UPDATE", string(id)).
			Scan(&locked),
	)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	published := make(chan error, 1)
	go func() { published <- workflow.PublishEvent(ctx, publisher, "approval:ordered", "on-time") }()
	require.Eventually(
		t,
		func() bool { return postgresApplicationWaiting(ctx, db, "chronicle-pending-publication") },
		time.Second,
		time.Millisecond,
	)
	clock.Set(deadline.Add(time.Second))
	runDone := make(chan error, 1)
	go func() { _, err := resumed.Run(ctx, id); runDone <- err }()
	require.Eventually(
		t,
		func() bool { return postgresApplicationWaiting(ctx, db, "chronicle-timeout-resume") },
		time.Second,
		time.Millisecond,
		"the timeout append must wait behind pending publication",
	)
	require.NoError(t, tx.Commit())
	select {
	case err := <-published:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("publication did not leave the row-lock wait")
	}
	select {
	case err := <-runDone:
		require.ErrorIs(t, err, workflow.ErrWorkflowInfrastructure)
	case <-ctx.Done():
		t.Fatal("timeout did not leave its publication-lock wait")
	}
	after, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(
		t,
		before.History,
		after.History,
		"an on-time publication must roll back the stale timeout decision",
	)
	require.Equal(t, 1, after.Summary.Attempt)
	out, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "on-time", *out)
}

func postgresApplicationDB(t *testing.T, database, applicationName string) *sql.DB {
	t.Helper()
	connectionURL, err := url.Parse(database)
	require.NoError(t, err)
	query := connectionURL.Query()
	query.Set("application_name", applicationName)
	connectionURL.RawQuery = query.Encode()
	return openDistributedDB(t, distributedPostgres, connectionURL.String())
}

func postgresApplicationWaiting(ctx context.Context, db *sql.DB, applicationName string) bool {
	var waiting bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", applicationName).
		Scan(&waiting)
	return err == nil && waiting
}
