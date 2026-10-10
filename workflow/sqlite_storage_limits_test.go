package workflow_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func storageLimitsDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=2000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSQLiteQueue_PositiveLeaseCannotWrapToExpiredOwnership(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db := storageLimitsDB(t, filepath.Join(t.TempDir(), "lease.db"))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	queue, err := workflow.NewSyncQueue(ctx, db,
		workflow.WithSyncQueueNowFunc(func() time.Time { return now }),
		workflow.WithSyncQueueLeaseDuration(time.Duration(math.MaxInt64)),
	)
	require.NoError(t, err)
	require.NoError(
		t,
		queue.Enqueue(
			ctx,
			workflow.QueuedTask{InstanceID: "lease-overflow", WorkflowName: "lease-overflow"},
		),
	)
	claim, err := queue.PollClaim(ctx)
	require.Error(t, err, "an unrepresentable deadline must fail before claiming the task")
	require.Nil(t, claim)

	healthy, err := workflow.NewSyncQueue(
		ctx,
		db,
		workflow.WithSyncQueueNowFunc(func() time.Time { return now }),
	)
	require.NoError(t, err)
	claim, err = healthy.PollClaim(ctx)
	require.NoError(t, err)
	require.NotNil(t, claim, "a rejected lease must leave the task available")
	require.Error(t, healthy.ExtendClaim(ctx, *claim, time.Duration(math.MaxInt64)))
	require.NoError(
		t,
		healthy.CompleteClaim(ctx, *claim),
		"a rejected renewal must preserve current ownership",
	)
}

func TestWorkflow_ConcurrentSQLiteRunnerInitialization(t *testing.T) {
	const callers = 8
	for iteration := range 8 {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		path := filepath.Join(t.TempDir(), fmt.Sprintf("startup-%d.db", iteration))
		// Establish WAL before concurrent initialization so first-open PRAGMA
		// contention cannot be mistaken for a migration failure.
		first := storageLimitsDB(t, path)
		if err := first.PingContext(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, callers)
		for range callers {
			db := storageLimitsDB(t, path)
			go func() {
				select {
				case <-start:
				case <-ctx.Done():
					results <- ctx.Err()
					return
				}
				_, err := workflow.NewSqliteRunnerWithSyncQueue(ctx, db)
				results <- err
			}()
		}
		close(start)
		var failures []error
		for range callers {
			select {
			case err := <-results:
				if err != nil {
					failures = append(failures, err)
				}
			case <-ctx.Done():
				cancel()
				t.Fatal("runner initialization did not finish within its bound")
			}
		}
		cancel()
		if len(failures) != 0 {
			t.Fatalf("concurrent startup iteration %d: %v", iteration, failures)
		}
	}
}
