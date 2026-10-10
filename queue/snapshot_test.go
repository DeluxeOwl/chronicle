package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/DeluxeOwl/chronicle/snapshotstore"
	"github.com/stretchr/testify/require"
)

func TestQueue_SnapshotSaveFailureDoesNotFailCommittedDelivery(t *testing.T) {
	log := eventlog.NewMemory()
	q, err := queue.New[string](log, "emails", queue.WithSnapshots(failingSnapshotStore{}, 1))
	require.NoError(t, err)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}),
	)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NoError(t, q.Ack(t.Context(), delivery.Receipt))

	reopened, err := queue.New[string](log, "emails")
	require.NoError(t, err)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}

type failingSnapshotStore struct{}

func (failingSnapshotStore) SaveSnapshot(context.Context, *queue.Snapshot) error {
	return errors.New("snapshot store unavailable")
}

func (failingSnapshotStore) GetSnapshot(context.Context, queue.ID) (*queue.Snapshot, bool, error) {
	return nil, false, nil
}

func TestQueue_RejectsInvalidSnapshotConfiguration(t *testing.T) {
	store := snapshotstore.NewMemory(func() *queue.Snapshot { return new(queue.Snapshot) })
	for _, option := range []queue.Option{queue.WithSnapshots(nil, 1), queue.WithSnapshots(store, 0)} {
		q, err := queue.New[string](eventlog.NewMemory(), "emails", option)
		require.Error(t, err)
		require.Nil(t, q)
	}
}

func TestQueue_SnapshotPreservesLeasesRetriesAndDeduplication(t *testing.T) {
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
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "in-flight", Payload: "send again"}),
	)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "retry", Payload: "send later"}),
	)

	done, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, done)
	require.NoError(t, q.Ack(t.Context(), done.Receipt))
	inFlight, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, inFlight)
	retry, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, retry)
	require.NoError(t, q.Nack(t.Context(), retry.Receipt, clock.Now().Add(2*time.Minute)))

	reopened, err := queue.New[string](
		log,
		"emails",
		queue.WithNowFunc(clock.Now),
		queue.WithSnapshots(store, 3),
	)
	require.NoError(t, err)
	require.NoError(t, reopened.Enqueue(t.Context(), finished))
	delivery, err := reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)

	clock.Advance(time.Minute)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "in-flight", delivery.Message.ID)
	require.Equal(t, uint64(2), delivery.Attempt)
	require.ErrorIs(t, reopened.Ack(t.Context(), inFlight.Receipt), queue.ErrLeaseLost)
	require.NoError(t, reopened.Ack(t.Context(), delivery.Receipt))

	clock.Advance(time.Minute)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "retry", delivery.Message.ID)
	require.Equal(t, uint64(2), delivery.Attempt)
	require.Equal(t, "send later", delivery.Message.Payload)
	require.NoError(t, reopened.Ack(t.Context(), delivery.Receipt))
}
