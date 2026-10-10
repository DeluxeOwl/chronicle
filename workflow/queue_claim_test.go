package workflow_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

type claimedQueueFactory struct {
	name string
	new  func(*testing.T, *controllableClock) workflow.ClaimedTaskQueue
}

func claimedQueueFactories() []claimedQueueFactory {
	return []claimedQueueFactory{
		{
			name: "memory",
			new: func(_ *testing.T, clock *controllableClock) workflow.ClaimedTaskQueue {
				return workflow.NewMemoryQueue(workflow.WithMemoryQueueNowFunc(clock.Now))
			},
		},
		{
			name: "sync",
			new: func(t *testing.T, clock *controllableClock) workflow.ClaimedTaskQueue {
				t.Helper()
				q, err := workflow.NewSyncQueue(t.Context(), setupSyncDB(t),
					workflow.WithSyncQueueNowFunc(clock.Now),
					workflow.WithSyncQueueWorkerID("shared-worker"),
				)
				require.NoError(t, err)
				return q
			},
		},
		{
			name: "async",
			new: func(t *testing.T, clock *controllableClock) workflow.ClaimedTaskQueue {
				t.Helper()
				q, err := workflow.NewAsyncQueue(t.Context(), setupAsyncDB(t),
					workflow.WithAsyncQueueNowFunc(clock.Now),
					workflow.WithAsyncQueueWorkerID("shared-worker"),
				)
				require.NoError(t, err)
				return q
			},
		},
	}
}

func TestClaimedQueue_StaleReceiptCannotAcknowledgeReclaimedTask(t *testing.T) {
	for _, factory := range claimedQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
			q := factory.new(t, clock)
			require.NoError(
				t,
				q.Enqueue(
					t.Context(),
					workflow.QueuedTask{InstanceID: "task", WorkflowName: "test"},
				),
			)

			old, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, old)
			clock.Advance(time.Minute)
			current, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, current)
			require.NotEqual(
				t,
				old.Receipt,
				current.Receipt,
				"each execution must get a distinct receipt",
			)

			require.ErrorIs(t, q.CompleteClaim(t.Context(), *old), workflow.ErrStaleClaim)
			require.ErrorIs(t, q.FailClaim(t.Context(), *old), workflow.ErrStaleClaim)
			require.ErrorIs(t, q.ExtendClaim(t.Context(), *old, time.Hour), workflow.ErrStaleClaim)
			require.NoError(t, q.ExtendClaim(t.Context(), *current, time.Minute))
			require.NoError(t, q.CompleteClaim(t.Context(), *current))
			remaining, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.Nil(t, remaining)
		})
	}
}

type sqliteQueueFactory struct {
	name string
	new  func(context.Context, *sql.DB) (workflow.ClaimedTaskQueue, error)
}

func sqliteQueueFactories() []sqliteQueueFactory {
	return []sqliteQueueFactory{
		{
			name: "sync",
			new: func(ctx context.Context, db *sql.DB) (workflow.ClaimedTaskQueue, error) {
				return workflow.NewSyncQueue(ctx, db)
			},
		},
		{
			name: "async",
			new: func(ctx context.Context, db *sql.DB) (workflow.ClaimedTaskQueue, error) {
				return workflow.NewAsyncQueue(ctx, db)
			},
		},
	}
}

func TestClaimedQueue_ExpiredReceiptCannotRenewOrAcknowledge(t *testing.T) {
	for _, factory := range claimedQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
			q := factory.new(t, clock)
			require.NoError(
				t,
				q.Enqueue(
					t.Context(),
					workflow.QueuedTask{InstanceID: "expired", WorkflowName: "test"},
				),
			)
			claim, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, claim)
			clock.Advance(30 * time.Second)
			require.ErrorIs(t, q.CompleteClaim(t.Context(), *claim), workflow.ErrStaleClaim)
			require.ErrorIs(t, q.FailClaim(t.Context(), *claim), workflow.ErrStaleClaim)
			require.ErrorIs(
				t,
				q.ExtendClaim(t.Context(), *claim, time.Minute),
				workflow.ErrStaleClaim,
			)
			reclaimed, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, reclaimed, "expired acknowledgments must leave the task recoverable")
		})
	}
}

func TestClaimedQueue_ReenqueueSurvivesOldCompletion(t *testing.T) {
	for _, factory := range claimedQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
			q := factory.new(t, clock)
			require.NoError(
				t,
				q.Enqueue(
					t.Context(),
					workflow.QueuedTask{InstanceID: "sleep", WorkflowName: "test"},
				),
			)
			old, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, old)
			wakeAt := clock.Now().Add(time.Hour)
			require.NoError(t, q.Enqueue(t.Context(), workflow.QueuedTask{
				InstanceID: "sleep", WorkflowName: "test", RunAfter: wakeAt,
			}))
			waiting, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.Nil(t, waiting, "enqueue must not revoke an active claim")
			require.NoError(t, q.CompleteClaim(t.Context(), *old))
			clock.Advance(2 * time.Hour)
			current, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(
				t,
				current,
				"acknowledging the old execution must preserve its follow-up",
			)
			require.WithinDuration(t, wakeAt, current.RunAfter, 0)
			require.ErrorIs(t, q.CompleteClaim(t.Context(), *old), workflow.ErrStaleClaim)
			require.NoError(t, q.CompleteClaim(t.Context(), *current))
		})
	}
}

func TestSQLiteQueue_UpgradesLegacyTablesWithoutLosingTasks(t *testing.T) {
	for _, factory := range sqliteQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			db := setupSyncDB(t)
			_, err := db.ExecContext(t.Context(), `
				CREATE TABLE workflow_ready_tasks (
					instance_id TEXT PRIMARY KEY, workflow_name TEXT NOT NULL,
					run_after_ns INTEGER NOT NULL DEFAULT 0, claimed_by TEXT, claimed_until_ns INTEGER
				);
				INSERT INTO workflow_ready_tasks (instance_id, workflow_name) VALUES ('legacy', 'test');
				CREATE TABLE workflow_published_events (event_name TEXT PRIMARY KEY, payload TEXT NOT NULL);
				INSERT INTO workflow_published_events VALUES ('old-event', '{}');
			`)
			require.NoError(t, err)
			q, err := factory.new(t.Context(), db)
			require.NoError(t, err)
			_, err = factory.new(t.Context(), db)
			require.NoError(t, err, "additive initialization must be idempotent")
			claim, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, claim)
			require.Equal(t, workflow.InstanceID("legacy"), claim.InstanceID)
			require.NoError(t, q.CompleteClaim(t.Context(), *claim))
			// This schema assertion is specific to the backward-compatible migration.
			var publishedAt int64
			err = db.QueryRowContext(t.Context(), `SELECT published_at_ns FROM workflow_published_events WHERE event_name = 'old-event'`).
				Scan(&publishedAt)
			require.NoError(t, err)
			require.Zero(t, publishedAt, "legacy timestamps remain unknown")
		})
	}
}

func TestSQLiteQueue_ConcurrentClaimsHaveExclusiveReceipts(t *testing.T) {
	for _, factory := range sqliteQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			db := setupSyncDB(t)
			_, err := db.ExecContext(t.Context(), `PRAGMA journal_mode=WAL`)
			require.NoError(t, err)
			db.SetMaxOpenConns(8)
			q, err := factory.new(t.Context(), db)
			require.NoError(t, err)
			const count = 32
			for i := range count {
				require.NoError(t, q.Enqueue(t.Context(), workflow.QueuedTask{
					InstanceID: workflow.InstanceID(
						fmt.Sprintf("task-%02d", i),
					), WorkflowName: "test",
				}))
			}
			start := make(chan struct{})
			claims := make(chan *workflow.ClaimedTask, count)
			errors := make(chan error, count)
			var workers sync.WaitGroup
			for range count {
				workers.Go(func() {
					<-start
					claim, err := q.PollClaim(t.Context())
					claims <- claim
					errors <- err
				})
			}
			close(start)
			workers.Wait()
			close(claims)
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
			seen := make(map[workflow.InstanceID]bool)
			receipts := make(map[workflow.ClaimReceipt]bool)
			for claim := range claims {
				require.NotNil(t, claim)
				require.False(t, seen[claim.InstanceID], "a task cannot have two live claims")
				require.False(t, receipts[claim.Receipt], "claims must not reuse a receipt")
				seen[claim.InstanceID] = true
				receipts[claim.Receipt] = true
			}
			require.Len(t, seen, count)
		})
	}
}
