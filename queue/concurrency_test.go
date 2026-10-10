package queue_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

// Forces two writers to decide from the same version before either can append.
type barrierLog struct {
	event.Log
	arrivals atomic.Int32
	gate     chan struct{}
}

func (l *barrierLog) AppendEvents(
	ctx context.Context,
	id event.LogID,
	check version.Check,
	events event.RawEvents,
) (version.Version, error) {
	arrival := l.arrivals.Add(1)
	if arrival == 2 {
		close(l.gate)
	}
	if arrival <= 2 {
		select {
		case <-l.gate:
		case <-ctx.Done():
			return version.Zero, ctx.Err()
		}
	}
	return l.Log.AppendEvents(ctx, id, check, events)
}

func TestQueue_ConcurrentWorkersDeliverEveryMessageOnceWhileLeasesAreActive(t *testing.T) {
	log := eventlog.NewMemory()
	q, err := queue.New[int](log, "numbers")
	require.NoError(t, err)
	const messages = 24
	const workers = 6
	for i := range messages {
		require.NoError(
			t,
			q.Enqueue(t.Context(), queue.Message[int]{ID: fmt.Sprintf("number-%d", i), Payload: i}),
		)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	delivered := make(chan *queue.Delivery[int], messages)
	failures := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		worker, err := queue.New[int](log, "numbers")
		require.NoError(t, err)
		wg.Go(func() {
			for {
				delivery, err := worker.Claim(ctx, time.Minute)
				if err != nil {
					failures <- err
					return
				}
				if delivery == nil {
					return
				}
				if err := worker.Ack(ctx, delivery.Receipt); err != nil {
					failures <- err
					return
				}
				delivered <- delivery
			}
		})
	}
	wg.Wait()
	close(delivered)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}

	seen := make(map[int]bool)
	for delivery := range delivered {
		require.False(
			t,
			seen[delivery.Message.Payload],
			"message delivered twice during its active lease",
		)
		seen[delivery.Message.Payload] = true
		require.Equal(t, uint64(1), delivery.Attempt)
	}
	require.Len(t, seen, messages)
	delivery, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}

func TestQueue_ConcurrentDuplicateEnqueueCreatesOneMessage(t *testing.T) {
	log := eventlog.NewMemory()
	barrier := &barrierLog{Log: log, arrivals: atomic.Int32{}, gate: make(chan struct{})}
	first, err := queue.New[string](barrier, "emails")
	require.NoError(t, err)
	second, err := queue.New[string](barrier, "emails")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, q := range []*queue.Persistent[string]{first, second} {
		go func() { results <- q.Enqueue(ctx, queue.Message[string]{ID: "same-id", Payload: "Welcome!"}) }()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	delivery, err := first.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.NoError(t, first.Ack(t.Context(), delivery.Receipt))
	delivery, err = second.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}
