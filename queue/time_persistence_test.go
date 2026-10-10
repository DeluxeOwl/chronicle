package queue_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/DeluxeOwl/chronicle/snapshotstore"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestQueue_ScheduledInstantAndEnqueueIdentitySurvivePersistence(t *testing.T) {
	testTimePersistenceBackends(
		t,
		func(t *testing.T, ctx context.Context, log event.Log, opts []queue.Option) {
			clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)}
			q := timePersistenceQueue(t, log, clock, opts)
			scheduled := time.Date(2026, 1, 1, 0, 1, 0, 123456789, time.UTC)
			msg := queue.Message[string]{
				ID: "same-id", Payload: "same-content",
				AvailableAt: scheduled.In(time.FixedZone("second-offset", 90)),
			}
			require.NoError(t, q.Enqueue(ctx, msg))
			require.NoError(t, q.Enqueue(ctx, msg), "an identical retry must remain idempotent")
			equivalent := msg
			equivalent.AvailableAt = scheduled.In(time.FixedZone("other-offset", -75))
			require.NoError(
				t,
				q.Enqueue(ctx, equivalent),
				"enqueue identity compares instants, not zones",
			)
			changed := msg
			changed.AvailableAt = changed.AvailableAt.Add(time.Nanosecond)
			require.ErrorIs(t, q.Enqueue(ctx, changed), queue.ErrMessageExists)

			clock.now = scheduled.Add(-time.Nanosecond)
			assertNoTimePersistenceDelivery(t, ctx, q)
			clock.now = scheduled
			delivery := claimTimePersistenceDelivery(t, ctx, q, 1)
			require.Equal(t, scheduled, delivery.Message.AvailableAt)
			require.Equal(t, "same-content", delivery.Message.Payload)
			require.NoError(t, q.Ack(ctx, delivery.Receipt))

			reopened := timePersistenceQueue(t, log, clock, opts)
			require.NoError(
				t,
				reopened.Enqueue(ctx, msg),
				"acknowledgment and reopening preserve the fingerprint",
			)
			require.NoError(t, reopened.Enqueue(ctx, equivalent))
			assertNoTimePersistenceDelivery(t, ctx, reopened)
		},
	)
}

func TestQueue_ClaimExpirySurvivesPersistenceWithSecondOffsetClock(t *testing.T) {
	testTimePersistenceBackends(
		t,
		func(t *testing.T, ctx context.Context, log event.Log, opts []queue.Option) {
			started := time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)
			clock := &testClock{now: started.In(time.FixedZone("second-offset", -90))}
			q := timePersistenceQueue(t, log, clock, opts)
			require.NoError(
				t,
				q.Enqueue(ctx, queue.Message[string]{ID: "message", Payload: "work"}),
			)
			first := claimTimePersistenceDelivery(t, ctx, q, 1)
			require.Equal(t, time.Time{}, first.Message.AvailableAt)
			require.Equal(t, time.Date(2026, 1, 1, 0, 1, 0, 123456789, time.UTC), first.LeaseUntil)

			clock.now = clock.now.Add(30 * time.Second)
			assertNoTimePersistenceDelivery(t, ctx, q)
			clock.now = first.LeaseUntil.Add(-time.Nanosecond).
				In(time.FixedZone("other-offset", 90))
			assertNoTimePersistenceDelivery(t, ctx, q)
			clock.now = first.LeaseUntil.In(time.FixedZone("other-offset", 90))
			require.ErrorIs(t, q.Ack(ctx, first.Receipt), queue.ErrLeaseLost)
			require.ErrorIs(t, q.Extend(ctx, first.Receipt, time.Minute), queue.ErrLeaseLost)
			require.ErrorIs(t, q.Nack(ctx, first.Receipt, time.Time{}), queue.ErrLeaseLost)
			second := claimTimePersistenceDelivery(t, ctx, q, 2)
			require.NotEqual(t, first.Receipt, second.Receipt)
			require.Equal(t, "work", second.Message.Payload)
			require.ErrorIs(t, q.Ack(ctx, first.Receipt), queue.ErrLeaseLost)
			require.NoError(t, q.Ack(ctx, second.Receipt))
			assertNoTimePersistenceDelivery(t, ctx, q)
		},
	)
}

func TestQueue_RetryAndExtendedLeaseInstantsSurvivePersistence(t *testing.T) {
	testTimePersistenceBackends(
		t,
		func(t *testing.T, ctx context.Context, log event.Log, opts []queue.Option) {
			clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 123456789, time.UTC)}
			q := timePersistenceQueue(t, log, clock, opts)
			msg := queue.Message[string]{ID: "message", Payload: "work"}
			require.NoError(t, q.Enqueue(ctx, msg))
			first := claimTimePersistenceDelivery(t, ctx, q, 1)
			retryAt := time.Date(2026, 1, 1, 0, 2, 0, 123456789, time.UTC)
			require.NoError(
				t,
				q.Nack(ctx, first.Receipt, retryAt.In(time.FixedZone("second-offset", 90))),
			)
			require.ErrorIs(t, q.Ack(ctx, first.Receipt), queue.ErrLeaseLost)
			require.NoError(
				t,
				q.Enqueue(ctx, msg),
				"producer retries must not reset the consumer's schedule",
			)
			clock.now = retryAt.Add(-time.Nanosecond)
			assertNoTimePersistenceDelivery(t, ctx, q)
			clock.now = retryAt
			second := claimTimePersistenceDelivery(t, ctx, q, 2)
			require.Equal(t, retryAt, second.Message.AvailableAt)

			clock.now = retryAt.Add(30 * time.Second).In(time.FixedZone("second-offset", -90))
			require.NoError(t, q.Extend(ctx, second.Receipt, 2*time.Minute))
			require.NoError(
				t,
				q.Extend(ctx, second.Receipt, time.Nanosecond),
				"extension must never shorten the lease",
			)
			clock.now = second.LeaseUntil
			assertNoTimePersistenceDelivery(t, ctx, q)
			extendedDeadline := time.Date(2026, 1, 1, 0, 4, 30, 123456789, time.UTC)
			clock.now = extendedDeadline.Add(-time.Nanosecond)
			assertNoTimePersistenceDelivery(t, ctx, q)
			clock.now = extendedDeadline
			require.ErrorIs(t, q.Extend(ctx, second.Receipt, time.Minute), queue.ErrLeaseLost)
			third := claimTimePersistenceDelivery(t, ctx, q, 3)
			require.NotEqual(t, second.Receipt, third.Receipt)
			require.NoError(t, q.Nack(ctx, third.Receipt, time.Time{}))
			fourth := claimTimePersistenceDelivery(t, ctx, q, 4)
			require.Equal(
				t,
				time.Time{},
				fourth.Message.AvailableAt,
				"zero retry time remains immediately eligible",
			)
			require.NoError(t, q.Ack(ctx, fourth.Receipt))
			require.NoError(t, q.Enqueue(ctx, msg))
			assertNoTimePersistenceDelivery(t, ctx, q)
		},
	)
}

func testTimePersistenceBackends(
	t *testing.T,
	run func(*testing.T, context.Context, event.Log, []queue.Option),
) {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "queue.db")+"?_busy_timeout=1000")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	sqliteLog, err := eventlog.NewSqlite(db)
	require.NoError(t, err)
	for name, log := range map[string]event.Log{"memory": eventlog.NewMemory(), "sqlite": sqliteLog} {
		t.Run(name, func(t *testing.T) {
			for _, snapshots := range []bool{false, true} {
				name := "event_replay"
				var opts []queue.Option
				if snapshots {
					name = "snapshot_replay"
					store := snapshotstore.NewMemory(
						func() *queue.Snapshot { return new(queue.Snapshot) },
					)
					opts = append(opts, queue.WithSnapshots(store, 1))
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					run(t, ctx, log, opts)
				})
			}
		})
	}
}

func timePersistenceQueue(
	t *testing.T,
	log event.Log,
	clock *testClock,
	opts []queue.Option,
) *queue.Persistent[string] {
	t.Helper()
	options := append([]queue.Option{queue.WithNowFunc(clock.Now)}, opts...)
	q, err := queue.New[string](log, t.Name(), options...)
	require.NoError(t, err)
	return q
}

func claimTimePersistenceDelivery(
	t *testing.T,
	ctx context.Context,
	q queue.Queue[string],
	attempt uint64,
) *queue.Delivery[string] {
	t.Helper()
	delivery, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, attempt, delivery.Attempt)
	return delivery
}

func assertNoTimePersistenceDelivery(t *testing.T, ctx context.Context, q queue.Queue[string]) {
	t.Helper()
	delivery, err := q.Claim(ctx, time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}
