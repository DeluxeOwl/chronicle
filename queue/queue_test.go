package queue_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func TestQueue_ScheduledAndLeasedMessagesDoNotBlockReadyWork(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	q := openQueue[string](t, eventlog.NewMemory(), "emails", clock.Now)
	for _, msg := range []queue.Message[string]{
		{ID: "scheduled", Payload: "later", AvailableAt: clock.Now().Add(time.Hour)},
		{ID: "first", Payload: "ready"},
		{ID: "second", Payload: "also ready"},
	} {
		require.NoError(t, q.Enqueue(t.Context(), msg))
	}
	first, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, "first", first.Message.ID)
	second, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, "second", second.Message.ID)
	require.NoError(t, q.Ack(t.Context(), first.Receipt))
	require.NoError(t, q.Ack(t.Context(), second.Receipt))
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
	clock.Advance(time.Hour)
	delivery, err = q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "scheduled", delivery.Message.ID)
}

func TestQueue_DifferentContentsCannotReplaceAnExistingMessage(t *testing.T) {
	q, err := queue.New[string](eventlog.NewMemory(), "emails")
	require.NoError(t, err)
	msg := queue.Message[string]{ID: "welcome-123", Payload: "original"}
	require.NoError(t, q.Enqueue(t.Context(), msg))
	require.ErrorIs(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: msg.ID, Payload: "changed"}),
		queue.ErrMessageExists,
	)
	require.ErrorIs(t, q.Enqueue(t.Context(), queue.Message[string]{
		ID: msg.ID, Payload: msg.Payload, AvailableAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}), queue.ErrMessageExists)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "original", delivery.Message.Payload)
}

func TestQueue_NamesAndReceiptsAreIsolated(t *testing.T) {
	log := eventlog.NewMemory()
	first, err := queue.New[string](log, "mail/send")
	require.NoError(t, err)
	second, err := queue.New[string](log, "mail_send")
	require.NoError(t, err)
	require.NoError(
		t,
		first.Enqueue(t.Context(), queue.Message[string]{ID: "same-id", Payload: "first queue"}),
	)
	require.NoError(
		t,
		second.Enqueue(t.Context(), queue.Message[string]{ID: "same-id", Payload: "second queue"}),
	)
	one, err := first.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, one)
	two, err := second.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, two)
	require.Equal(t, "first queue", one.Message.Payload)
	require.Equal(t, "second queue", two.Message.Payload)
	require.ErrorIs(t, second.Ack(t.Context(), one.Receipt), queue.ErrLeaseLost)
	require.NoError(t, second.Ack(t.Context(), two.Receipt))
	require.NoError(t, first.Ack(t.Context(), one.Receipt))
}

func TestQueue_ProtobufPointerPayloadCanBeClaimed(t *testing.T) {
	q, err := queue.New[*wrapperspb.StringValue](
		eventlog.NewMemory(),
		"protobuf",
		queue.WithCodec(encoding.NewProtobuf()),
		queue.WithPayloadFactory(
			func() *wrapperspb.StringValue { return new(wrapperspb.StringValue) },
		),
	)
	require.NoError(t, err)
	require.NoError(t, q.Enqueue(t.Context(), queue.Message[*wrapperspb.StringValue]{
		ID: "welcome-123", Payload: wrapperspb.String("Welcome!"),
	}))
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NotNil(t, delivery.Message.Payload)
	require.Equal(t, "Welcome!", delivery.Message.Payload.GetValue())
	require.NoError(t, q.Ack(t.Context(), delivery.Receipt))
}

func TestQueue_CustomPayloadCodec(t *testing.T) {
	q, err := queue.New[[]byte](
		eventlog.NewMemory(),
		"uploads",
		queue.WithCodec(encoding.NewGeneric(
			func(value any) ([]byte, error) { return bytes.Clone(value.([]byte)), nil },
			func(data []byte, target any) error { *target.(*[]byte) = bytes.Clone(data); return nil },
		)),
	)
	require.NoError(t, err)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[[]byte]{ID: "blob-123", Payload: []byte{0, 1, 255}}),
	)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, []byte{0, 1, 255}, delivery.Message.Payload)
}

func TestQueue_ExtendKeepsLeaseAliveWithoutShorteningIt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	q := openQueue[string](t, eventlog.NewMemory(), "emails", clock.Now)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}),
	)
	first, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)

	clock.Advance(30 * time.Second)
	require.NoError(t, q.Extend(t.Context(), first.Receipt, 2*time.Minute))
	require.NoError(t, q.Extend(t.Context(), first.Receipt, time.Second))
	clock.Advance(time.Minute)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery, "a short heartbeat must not shorten the existing lease")

	clock.Advance(time.Minute)
	require.ErrorIs(t, q.Extend(t.Context(), first.Receipt, time.Minute), queue.ErrLeaseLost)
	require.ErrorIs(t, q.Nack(t.Context(), first.Receipt, time.Time{}), queue.ErrLeaseLost)
	second, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.ErrorIs(t, q.Extend(t.Context(), first.Receipt, time.Minute), queue.ErrLeaseLost)
	require.ErrorIs(t, q.Nack(t.Context(), first.Receipt, time.Time{}), queue.ErrLeaseLost)
	require.NoError(t, q.Ack(t.Context(), second.Receipt))
}

func TestQueue_NackSchedulesAnotherAttempt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	q := openQueue[string](t, eventlog.NewMemory(), "emails", clock.Now)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}),
	)
	first, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)

	retryAt := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	require.NoError(t, q.Nack(t.Context(), first.Receipt, retryAt))
	require.ErrorIs(t, q.Ack(t.Context(), first.Receipt), queue.ErrLeaseLost)
	clock.Advance(4 * time.Minute)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)

	clock.Advance(time.Minute)
	delivery, err = q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, uint64(2), delivery.Attempt)
	require.Equal(t, retryAt, delivery.Message.AvailableAt)
	require.NotEqual(t, first.Receipt, delivery.Receipt)
	require.NoError(t, q.Nack(t.Context(), delivery.Receipt, time.Time{}))
	retry, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, retry, "a zero retry time makes work immediately eligible")
	require.Equal(t, uint64(3), retry.Attempt)
}

func TestQueue_ExpiredLeaseIsRedeliveredWithANewReceipt(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	log := eventlog.NewMemory()
	q := openQueue[string](t, log, "emails", clock.Now)
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}),
	)
	first, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.Equal(t, time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC), first.LeaseUntil)

	clock.Advance(59 * time.Second)
	unavailable, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, unavailable)

	clock.Advance(time.Second)
	require.ErrorIs(t, q.Ack(t.Context(), first.Receipt), queue.ErrLeaseLost)
	other := openQueue[string](t, log, "emails", clock.Now)
	second, err := other.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, "welcome-123", second.Message.ID)
	require.Equal(t, uint64(2), second.Attempt)
	require.NotEqual(t, first.Receipt, second.Receipt)
	require.ErrorIs(t, q.Ack(t.Context(), first.Receipt), queue.ErrLeaseLost)
	require.NoError(t, q.Ack(t.Context(), second.Receipt))
}

type testClock struct {
	now time.Time
}

func (c *testClock) Now() time.Time          { return c.now }
func (c *testClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func openQueue[T any](
	t *testing.T,
	log event.Log,
	name string,
	now func() time.Time,
) *queue.Persistent[T] {
	t.Helper()
	q, err := queue.New[T](log, name, queue.WithNowFunc(now))
	require.NoError(t, err)
	return q
}

func TestQueue_EnqueueIsIdempotentAfterAcknowledgment(t *testing.T) {
	log := eventlog.NewMemory()
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	q := openQueue[string](t, log, "emails", clock.Now)
	msg := queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}
	require.NoError(t, q.Enqueue(t.Context(), msg))
	require.NoError(t, q.Enqueue(t.Context(), msg))
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NoError(t, q.Ack(t.Context(), delivery.Receipt))

	clock.Advance(2 * time.Minute)
	reopened := openQueue[string](t, log, "emails", clock.Now)
	require.NoError(t, reopened.Enqueue(t.Context(), msg))
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery, "a producer retry must not resurrect completed work")
}
