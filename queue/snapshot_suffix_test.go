package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/DeluxeOwl/chronicle/snapshotstore"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

func TestQueue_SnapshotsReadOnlyTheEventSuffix(t *testing.T) {
	log := eventlog.NewMemory()
	store := snapshotstore.NewMemory(func() *queue.Snapshot { return new(queue.Snapshot) })
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	q, err := queue.New[string](
		log,
		"emails",
		queue.WithNowFunc(clock.Now),
		queue.WithSnapshots(store, 3),
	)
	require.NoError(t, err)
	finished := queue.Message[string]{ID: "finished", Payload: "already sent"}
	require.NoError(t, q.Enqueue(t.Context(), finished))
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NoError(t, q.Ack(t.Context(), delivery.Receipt))

	// The first three events are in a snapshot. The fourth is a live suffix.
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "pending", Payload: "send next"}),
	)
	suffix := &suffixOnlyLog{Log: log, t: t, from: 4}
	reopened, err := queue.New[string](
		suffix,
		"emails",
		queue.WithNowFunc(clock.Now),
		queue.WithSnapshots(store, 3),
	)
	require.NoError(t, err)
	require.NoError(t, reopened.Enqueue(t.Context(), finished))
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "pending", delivery.Message.ID)
	require.Equal(t, "send next", delivery.Message.Payload)
	require.Equal(t, uint64(1), delivery.Attempt)
	require.NoError(t, reopened.Ack(t.Context(), delivery.Receipt))
	clock.Advance(2 * time.Minute)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}

// This log rejects full-history reads, so a snapshot no-op cannot pass the test.
type suffixOnlyLog struct {
	event.Log
	t    *testing.T
	from version.Version
}

func (l *suffixOnlyLog) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	l.t.Helper()
	require.GreaterOrEqual(
		l.t,
		selector.From,
		l.from,
		"snapshot loads must skip the snapshotted prefix",
	)
	return l.Log.ReadEvents(ctx, id, selector)
}
