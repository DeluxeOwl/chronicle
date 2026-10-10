package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

func TestQueue_InvalidUTF8MessageIDsAreRejectedWithoutCorruptingWork(t *testing.T) {
	q, err := queue.New[string](eventlog.NewMemory(), "emails")
	require.NoError(t, err)
	for _, id := range []string{"\xff", "\xfe", "invalid\xffid"} {
		require.Error(t, q.Enqueue(t.Context(), queue.Message[string]{ID: id, Payload: "invalid"}))
	}
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "welcôme-123", Payload: "Welcome!"}),
	)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "welcôme-123", delivery.Message.ID)
	require.Equal(t, "Welcome!", delivery.Message.Payload)
}

func TestQueue_CancellationDuringConflictRetryDoesNotClaimWork(t *testing.T) {
	log := eventlog.NewMemory()
	producer, err := queue.New[string](log, "emails")
	require.NoError(t, err)
	require.NoError(
		t,
		producer.Enqueue(
			t.Context(),
			queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"},
		),
	)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conflicting := &cancelOnConflictLog{Log: log, cancel: cancel}
	worker, err := queue.New[string](conflicting, "emails")
	require.NoError(t, err)
	delivery, err := worker.Claim(ctx, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, delivery)

	delivery, err = producer.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, uint64(1), delivery.Attempt)
}

type cancelOnConflictLog struct {
	event.Log
	cancel context.CancelFunc
}

func (l *cancelOnConflictLog) AppendEvents(
	_ context.Context,
	_ event.LogID,
	check version.Check,
	_ event.RawEvents,
) (version.Version, error) {
	l.cancel()
	expected := version.Version(check.(version.CheckExact))
	return version.Zero, version.NewConflictError(expected, expected+1)
}

func TestQueue_EnqueueRetryPreservesAScheduledRetry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	q := openQueue[string](t, eventlog.NewMemory(), "emails", clock.Now)
	msg := queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}
	require.NoError(t, q.Enqueue(t.Context(), msg))
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NoError(t, q.Nack(t.Context(), delivery.Receipt, clock.Now().Add(time.Hour)))
	require.NoError(t, q.Enqueue(t.Context(), msg))
	delivery, err = q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery, "a producer retry must not reset the consumer's retry schedule")
}

func TestQueue_InvalidCommandsLeaveMessagesAvailable(t *testing.T) {
	q, err := queue.New[string](eventlog.NewMemory(), "emails")
	require.NoError(t, err)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}),
	)
	for _, lease := range []time.Duration{0, -time.Second} {
		delivery, err := q.Claim(t.Context(), lease)
		require.Error(t, err)
		require.Nil(t, delivery)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	delivery, err := q.Claim(ctx, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, delivery)
	delivery, err = q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Error(t, q.Extend(t.Context(), delivery.Receipt, 0))
	require.ErrorIs(t, q.Ack(t.Context(), queue.Receipt("unknown")), queue.ErrLeaseLost)
	require.ErrorIs(t, q.Ack(ctx, delivery.Receipt), context.Canceled)
	require.NoError(t, q.Ack(t.Context(), delivery.Receipt))
}

func TestQueue_FailedWritesDoNotLoseWork(t *testing.T) {
	log := eventlog.NewMemory()
	producer, err := queue.New[string](log, "emails")
	require.NoError(t, err)
	require.NoError(
		t,
		producer.Enqueue(
			t.Context(),
			queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"},
		),
	)

	failure := errors.New("storage unavailable")
	unavailable := &failingLog{Log: log, err: failure}
	worker, err := queue.New[string](unavailable, "emails")
	require.NoError(t, err)
	delivery, err := worker.Claim(t.Context(), time.Minute)
	require.ErrorIs(t, err, failure)
	require.Nil(t, delivery, "failed claims must not escape to the handler")

	delivery, err = producer.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, uint64(1), delivery.Attempt)
	require.ErrorIs(t, worker.Ack(t.Context(), delivery.Receipt), failure)
	require.NoError(t, producer.Ack(t.Context(), delivery.Receipt))
}

func TestQueue_AmbiguousEnqueueCanBeRetriedWithoutDuplication(t *testing.T) {
	log := eventlog.NewMemory()
	failure := errors.New("connection lost after commit")
	ambiguous := &ambiguousLog{Log: log, err: failure}
	q, err := queue.New[string](ambiguous, "emails")
	require.NoError(t, err)
	msg := queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}
	require.ErrorIs(t, q.Enqueue(t.Context(), msg), failure)

	reopened, err := queue.New[string](log, "emails")
	require.NoError(t, err)
	require.NoError(t, reopened.Enqueue(t.Context(), msg))
	delivery, err := reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NoError(t, reopened.Ack(t.Context(), delivery.Receipt))
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}

func TestQueue_PayloadDecodeFailureDoesNotClaimTheMessage(t *testing.T) {
	log := eventlog.NewMemory()
	producer, err := queue.New[string](log, "emails")
	require.NoError(t, err)
	require.NoError(
		t,
		producer.Enqueue(
			t.Context(),
			queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"},
		),
	)
	failure := errors.New("unsupported payload schema")
	worker, err := queue.New[string](log, "emails", queue.WithCodec(encoding.NewGeneric(
		encoding.DefaultJSONB.Encode,
		func([]byte, any) error { return failure },
	)))
	require.NoError(t, err)
	delivery, err := worker.Claim(t.Context(), time.Minute)
	require.ErrorIs(t, err, failure)
	require.Nil(t, delivery)
	delivery, err = producer.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, uint64(1), delivery.Attempt)
}

type failingLog struct {
	event.Log
	err error
}

func (l *failingLog) AppendEvents(
	context.Context,
	event.LogID,
	version.Check,
	event.RawEvents,
) (version.Version, error) {
	return version.Zero, l.err
}

type ambiguousLog struct {
	event.Log
	err error
}

func (l *ambiguousLog) AppendEvents(
	ctx context.Context,
	id event.LogID,
	check version.Check,
	events event.RawEvents,
) (version.Version, error) {
	v, err := l.Log.AppendEvents(ctx, id, check, events)
	if err != nil {
		return v, err
	}
	return version.Zero, l.err
}
