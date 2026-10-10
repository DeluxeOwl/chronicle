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

type clockedSQLiteQueueFactory struct {
	name string
	new  func(context.Context, *sql.DB, func() time.Time) (workflow.ClaimedTaskQueue, error)
}

func clockedSQLiteQueueFactories() []clockedSQLiteQueueFactory {
	return []clockedSQLiteQueueFactory{
		{
			name: "sync",
			new: func(ctx context.Context, db *sql.DB, now func() time.Time) (workflow.ClaimedTaskQueue, error) {
				return workflow.NewSyncQueue(ctx, db, workflow.WithSyncQueueNowFunc(now))
			},
		},
		{
			name: "async",
			new: func(ctx context.Context, db *sql.DB, now func() time.Time) (workflow.ClaimedTaskQueue, error) {
				return workflow.NewAsyncQueue(ctx, db, workflow.WithAsyncQueueNowFunc(now))
			},
		},
	}
}

func TestSQLiteQueue_ExpiryIsCheckedAfterWaitingForAConnection(t *testing.T) {
	operations := []struct {
		name string
		run  func(context.Context, workflow.ClaimedTaskQueue, workflow.ClaimedTask) error
	}{
		{
			name: "complete",
			run: func(ctx context.Context, q workflow.ClaimedTaskQueue, claim workflow.ClaimedTask) error {
				return q.CompleteClaim(ctx, claim)
			},
		},
		{
			name: "renew",
			run: func(ctx context.Context, q workflow.ClaimedTaskQueue, claim workflow.ClaimedTask) error {
				return q.ExtendClaim(ctx, claim, time.Minute)
			},
		},
		{
			name: "release",
			run: func(ctx context.Context, q workflow.ClaimedTaskQueue, claim workflow.ClaimedTask) error {
				return q.(workflow.ClaimedTaskReleaser).ReleaseClaim(ctx, claim, time.Time{})
			},
		},
	}
	for _, factory := range clockedSQLiteQueueFactories() {
		for _, operation := range operations {
			t.Run(factory.name+"/"+operation.name, func(t *testing.T) {
				db := setupSyncDB(t)
				db.SetMaxOpenConns(1)
				clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
				q, err := factory.new(t.Context(), db, clock.Now)
				require.NoError(t, err)
				require.NoError(
					t,
					q.Enqueue(
						t.Context(),
						workflow.QueuedTask{InstanceID: "contended", WorkflowName: "test"},
					),
				)
				claim, err := q.PollClaim(t.Context())
				require.NoError(t, err)
				require.NotNil(t, claim)
				clock.Advance(29 * time.Second)
				connection, err := db.Conn(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { _ = connection.Close() })
				waitCount := db.Stats().WaitCount
				done := make(chan error, 1)
				go func() { done <- operation.run(t.Context(), q, *claim) }()
				require.Eventually(
					t,
					func() bool { return db.Stats().WaitCount > waitCount },
					time.Second,
					time.Millisecond,
					"operation must wait before the clock crosses expiry",
				)
				clock.Advance(2 * time.Second)
				require.NoError(t, connection.Close())
				select {
				case err := <-done:
					require.ErrorIs(t, err, workflow.ErrStaleClaim)
				case <-time.After(time.Second):
					t.Fatal("claim operation did not resume")
				}
				reclaimed, err := q.PollClaim(t.Context())
				require.NoError(t, err)
				require.NotNil(
					t,
					reclaimed,
					"contention must not make expired ownership destructive",
				)
				require.NotEqual(t, claim.Receipt, reclaimed.Receipt)
			})
		}
	}
}

func TestSQLiteQueue_ExpiryIsCheckedAfterWaitingForAWriteLock(t *testing.T) {
	for _, factory := range clockedSQLiteQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "contended.db")
			open := func() *sql.DB {
				db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=2000")
				require.NoError(t, err)
				db.SetMaxOpenConns(1)
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			db := open()
			other := open()
			clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			observed := make(chan struct{}, 4)
			q, err := factory.new(t.Context(), db, func() time.Time {
				select {
				case observed <- struct{}{}:
				default:
				}
				return clock.Now()
			})
			require.NoError(t, err)
			require.NoError(
				t,
				q.Enqueue(
					t.Context(),
					workflow.QueuedTask{InstanceID: "write-contended", WorkflowName: "test"},
				),
			)
			claim, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, claim)
			for len(observed) > 0 {
				<-observed
			}
			clock.Advance(29 * time.Second)
			tx, err := other.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			_, err = tx.ExecContext(
				t.Context(),
				"UPDATE workflow_ready_tasks SET claimed_by=claimed_by",
			)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { done <- q.ExtendClaim(t.Context(), *claim, time.Minute) }()
			// Connection in use proves the renewal reached storage, not merely
			// a goroutine launch. The held write transaction blocks its mutation.
			require.Eventually(
				t,
				func() bool { return db.Stats().InUse == 1 },
				time.Second,
				time.Millisecond,
			)
			// Under the faulty implementation the clock is read before locking.
			// Give that call a chance to execute, without requiring the fixed
			// implementation to read time while the lock is held.
			select {
			case <-observed:
			case <-time.After(20 * time.Millisecond):
			}
			clock.Advance(2 * time.Second)
			require.NoError(t, tx.Commit())
			select {
			case err := <-done:
				require.ErrorIs(t, err, workflow.ErrStaleClaim)
			case <-time.After(3 * time.Second):
				t.Fatal("blocked renewal did not resume")
			}
			reclaimed, err := q.PollClaim(t.Context())
			require.NoError(t, err)
			require.NotNil(t, reclaimed)
		})
	}
}

func TestSQLiteQueue_ClaimLeaseStartsAfterConnectionWait(t *testing.T) {
	for _, factory := range clockedSQLiteQueueFactories() {
		t.Run(factory.name, func(t *testing.T) {
			db := setupSyncDB(t)
			db.SetMaxOpenConns(1)
			clock := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			q, err := factory.new(t.Context(), db, clock.Now)
			require.NoError(t, err)
			require.NoError(
				t,
				q.Enqueue(
					t.Context(),
					workflow.QueuedTask{InstanceID: "claim-after-wait", WorkflowName: "test"},
				),
			)
			connection, err := db.Conn(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { _ = connection.Close() })
			waitCount := db.Stats().WaitCount
			type result struct {
				claim *workflow.ClaimedTask
				err   error
			}
			done := make(chan result, 1)
			go func() { claim, err := q.PollClaim(t.Context()); done <- result{claim: claim, err: err} }()
			require.Eventually(
				t,
				func() bool { return db.Stats().WaitCount > waitCount },
				time.Second,
				time.Millisecond,
			)
			clock.Advance(time.Minute)
			require.NoError(t, connection.Close())
			select {
			case claimed := <-done:
				require.NoError(t, claimed.err)
				require.NotNil(t, claimed.claim)
				require.NoError(
					t,
					q.CompleteClaim(t.Context(), *claimed.claim),
					"a newly returned claim must not be expired from connection wait time",
				)
			case <-time.After(time.Second):
				t.Fatal("claim did not resume")
			}
		})
	}
}
