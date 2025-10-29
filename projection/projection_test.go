package projection_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/projection"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/require"
)

// In-memory implementation of Checkpointer for testing.
type memoryCheckpointer struct {
	mu        sync.Mutex
	versions  map[string]version.Version
	saveCalls int
	getCalls  int
	lastSaved version.Version
}

func (m *memoryCheckpointer) GetCheckpoint(
	_ context.Context,
	projectionName string,
) (version.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	return m.versions[projectionName], nil
}

func (m *memoryCheckpointer) SaveCheckpoint(
	_ context.Context,
	projectionName string,
	v version.Version,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls++
	m.versions[projectionName] = v
	m.lastSaved = v
	return nil
}

func TestProjectionGroup_RunAndShutdown(t *testing.T) {
	ctx := t.Context()
	// Setup
	log := eventlog.NewMemory()

	//nolint:exhaustruct // not needed.
	checkpointer := &memoryCheckpointer{versions: make(map[string]version.Version)}
	pollInterval := 50 * time.Millisecond

	// Use a channel to receive handled events for synchronization.
	handledEvents := make(chan *event.GlobalRecord, 10)

	// Mock projection that sends handled events to our channel.
	accountProjection := &ProjectionMock{
		NameFunc: func() string { return "accounts" },
		EventNamesFunc: func() []string {
			return []string{"account/opened", "account/closed"}
		},
		HandleFunc: func(_ context.Context, rec *event.GlobalRecord) error {
			handledEvents <- rec
			return nil
		},
	}

	// Create and run the projection group in a background goroutine.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	group := projection.NewGroup(
		log,
		checkpointer,
		[]projection.Projection{accountProjection},
		projection.WithPollInterval(pollInterval),
	)
	go group.Run(ctx)

	_, err := log.AppendEvents(
		ctx,
		event.LogID("account-1"),
		version.CheckExact(0),
		event.RawEvents{
			event.NewRaw("account/opened", []byte(`{}`)),
		},
	)
	require.NoError(t, err)

	_, err = log.AppendEvents(ctx, event.LogID("order-1"), version.CheckExact(0), event.RawEvents{
		event.NewRaw("order/placed", []byte(`{}`)), // This should be ignored
	})
	require.NoError(t, err)

	_, err = log.AppendEvents(ctx, event.LogID("account-2"), version.CheckExact(0), event.RawEvents{
		event.NewRaw("account/opened", []byte(`{}`)),
	})
	require.NoError(t, err)

	// Assert: Wait for the projection to handle the events.
	var received []*event.GlobalRecord

	for range 2 {
		rec := <-handledEvents
		received = append(received, rec)
	}

	// Verify the correct events were handled.
	require.Len(t, received, 2)
	require.Equal(t, version.Version(1), received[0].GlobalVersion()) // Event 1
	require.Equal(t, version.Version(3), received[1].GlobalVersion()) // Event 3

	require.Eventually(t, func() bool {
		checkpointer.mu.Lock()
		defer checkpointer.mu.Unlock()
		return checkpointer.lastSaved == version.Version(3)
	}, 1*time.Second, pollInterval, "checkpoint should have been saved at version 3")

	cancel()
	err = group.Wait()

	require.NoError(t, err, "group.Wait() should return no error on clean shutdown")

	require.Len(t, accountProjection.HandleCalls(), 2)
}
