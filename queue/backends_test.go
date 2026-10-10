package queue_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/internal/testutils"
	"github.com/DeluxeOwl/chronicle/queue"
	"github.com/stretchr/testify/require"
)

func TestQueue_CompetingClaimsAcrossEventLogs(t *testing.T) {
	logs, cleanup := testutils.SetupEventLogs(t)
	defer cleanup()
	for _, backend := range logs {
		t.Run(backend.Name, func(t *testing.T) {
			q, err := queue.New[string](backend.Log, "competing-workers")
			require.NoError(t, err)
			require.NoError(
				t,
				q.Enqueue(
					t.Context(),
					queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"},
				),
			)
			barrier := &barrierLog{
				Log:      backend.Log,
				arrivals: atomic.Int32{},
				gate:     make(chan struct{}),
			}
			first, err := queue.New[string](barrier, "competing-workers")
			require.NoError(t, err)
			second, err := queue.New[string](barrier, "competing-workers")
			require.NoError(t, err)
			type result struct {
				delivery *queue.Delivery[string]
				err      error
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			results := make(chan result, 2)
			for _, worker := range []*queue.Persistent[string]{first, second} {
				go func() {
					delivery, err := worker.Claim(ctx, time.Minute)
					results <- result{delivery: delivery, err: err}
				}()
			}
			var receipts []queue.Receipt
			for range 2 {
				outcome := <-results
				require.NoError(t, outcome.err)
				if outcome.delivery != nil {
					receipts = append(receipts, outcome.delivery.Receipt)
				}
			}
			require.Len(t, receipts, 1, "the losing claim must not return an uncommitted delivery")
			require.NoError(t, q.Ack(t.Context(), receipts[0]))
		})
	}
}

func TestQueue_LifecycleAcrossEventLogs(t *testing.T) {
	logs, cleanup := testutils.SetupEventLogs(t)
	defer cleanup()
	for _, backend := range logs {
		t.Run(backend.Name, func(t *testing.T) {
			clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			q := openQueue[string](t, backend.Log, "emails / account.created", clock.Now)
			message := queue.Message[string]{ID: "welcome-123", Payload: "Welcome!"}
			require.NoError(t, q.Enqueue(t.Context(), message))
			first, err := q.Claim(t.Context(), time.Minute)
			require.NoError(t, err)
			require.NotNil(t, first)
			require.Equal(t, "Welcome!", first.Message.Payload)
			require.NoError(t, q.Nack(t.Context(), first.Receipt, clock.Now().Add(time.Minute)))

			other := openQueue[string](t, backend.Log, "emails / account.created", clock.Now)
			delivery, err := other.Claim(t.Context(), time.Minute)
			require.NoError(t, err)
			require.Nil(t, delivery)
			clock.Advance(time.Minute)
			delivery, err = other.Claim(t.Context(), time.Minute)
			require.NoError(t, err)
			require.NotNil(t, delivery)
			require.Equal(t, uint64(2), delivery.Attempt)
			require.ErrorIs(t, q.Ack(t.Context(), first.Receipt), queue.ErrLeaseLost)
			require.NoError(t, other.Extend(t.Context(), delivery.Receipt, 2*time.Minute))
			require.NoError(t, q.Ack(t.Context(), delivery.Receipt))
			clock.Advance(3 * time.Minute)
			require.NoError(t, other.Enqueue(t.Context(), message))
			delivery, err = q.Claim(t.Context(), time.Minute)
			require.NoError(t, err)
			require.Nil(t, delivery)
		})
	}
}
