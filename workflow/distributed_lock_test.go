//go:build !windows

package workflow_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func distributedClaimExpiryAfterLockWait(t *testing.T, backend, database string) {
	if backend != distributedPostgres {
		t.Skip("SQLite connection and write-lock expiry have fake-clock regressions")
	}
	db := openDistributedDB(t, backend, database)
	queue, err := workflow.NewPostgresQueue(
		t.Context(),
		db,
		workflow.WithPostgresQueueLeaseDuration(500*time.Millisecond),
	)
	require.NoError(t, err)
	operations := []struct {
		name string
		run  func(context.Context, *workflow.PostgresQueue, workflow.ClaimedTask) error
	}{
		{
			name: "complete",
			run: func(ctx context.Context, q *workflow.PostgresQueue, task workflow.ClaimedTask) error {
				return q.CompleteClaim(ctx, task)
			},
		},
		{
			name: "extend",
			run: func(ctx context.Context, q *workflow.PostgresQueue, task workflow.ClaimedTask) error {
				return q.ExtendClaim(ctx, task, time.Second)
			},
		},
		{
			name: "release",
			run: func(ctx context.Context, q *workflow.PostgresQueue, task workflow.ClaimedTask) error {
				return q.ReleaseClaim(ctx, task, time.Time{})
			},
		},
	}
	for index, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			applicationName := fmt.Sprintf("chronicle-claim-lock-%d", index)
			connectionURL, err := url.Parse(database)
			require.NoError(t, err)
			query := connectionURL.Query()
			query.Set("application_name", applicationName)
			connectionURL.RawQuery = query.Encode()
			client := openDistributedDB(t, backend, connectionURL.String())
			clientQueue, err := workflow.NewPostgresQueue(t.Context(), client)
			require.NoError(t, err)
			id := workflow.InstanceID(fmt.Sprintf("locked-task-%d", index))
			require.NoError(
				t,
				queue.Enqueue(
					t.Context(),
					workflow.QueuedTask{InstanceID: id, WorkflowName: "test"},
				),
			)
			claim, err := queue.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, claim)
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			var locked string
			require.NoError(
				t,
				tx.QueryRowContext(t.Context(), "SELECT instance_id FROM workflow_ready_tasks WHERE instance_id=$1 FOR UPDATE", string(id)).
					Scan(&locked),
			)
			var unexpired bool
			require.NoError(
				t,
				tx.QueryRowContext(t.Context(), "SELECT claimed_until_ns > (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000000)::bigint FROM workflow_ready_tasks WHERE instance_id=$1", string(id)).
					Scan(&unexpired),
			)
			require.True(t, unexpired, "the operation must begin before expiry")
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- operation.run(ctx, clientQueue, *claim) }()
			require.Eventually(t, func() bool {
				var waiting bool
				err := db.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", applicationName).
					Scan(&waiting)
				return err == nil && waiting
			}, time.Second, time.Millisecond, "the operation must actually wait for the task lock")
			require.Eventually(t, func() bool {
				var expired bool
				err := tx.QueryRowContext(t.Context(), "SELECT claimed_until_ns <= (EXTRACT(EPOCH FROM clock_timestamp()) * 1000000000)::bigint FROM workflow_ready_tasks WHERE instance_id=$1", string(id)).
					Scan(&expired)
				return err == nil && expired
			}, time.Second, 5*time.Millisecond)
			require.NoError(t, tx.Commit())
			select {
			case err := <-done:
				require.ErrorIs(t, err, workflow.ErrStaleClaim)
			case <-ctx.Done():
				t.Fatal("claim operation failed to leave its lock wait")
			}
			reclaimed, err := queue.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, reclaimed)
			require.NotEqual(t, claim.Receipt, reclaimed.Receipt)
			require.NoError(t, queue.CompleteClaim(t.Context(), *reclaimed))
		})
	}
}
