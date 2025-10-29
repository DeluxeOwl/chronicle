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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type memoryCheckpointer struct {
	mu        sync.Mutex
	versions  map[string]version.Version
	saveCalls map[string]int
	getCalls  map[string]int
	lastSaved map[string]version.Version
}

//nolint:exhaustruct // not needed.
func newMemoryCheckpointer() *memoryCheckpointer {
	return &memoryCheckpointer{
		versions:  make(map[string]version.Version),
		saveCalls: make(map[string]int),
		getCalls:  make(map[string]int),
		lastSaved: make(map[string]version.Version),
	}
}

func (m *memoryCheckpointer) GetCheckpoint(
	_ context.Context,
	projectionName string,
) (version.Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls[projectionName]++
	return m.versions[projectionName], nil
}

func (m *memoryCheckpointer) SaveCheckpoint(
	_ context.Context,
	projectionName string,
	v version.Version,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalls[projectionName]++
	m.versions[projectionName] = v
	m.lastSaved[projectionName] = v
	return nil
}

func TestProjectionGroup_RunAndShutdown_WithGlob(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	log := eventlog.NewMemory()
	checkpointer := newMemoryCheckpointer()
	pollInterval := 50 * time.Millisecond

	handledEvents := make(chan *event.GlobalRecord, 10)

	accountProjection := &ProjectionMock{
		NameFunc: func() string { return "accounts" },
		EventNamesFunc: func() []string {
			return []string{"account.*"}
		},
		HandleFunc: func(_ context.Context, rec *event.GlobalRecord) error {
			handledEvents <- rec
			return nil
		},
	}

	group, err := projection.NewGroup(
		log,
		checkpointer,
		[]projection.Projection{accountProjection},
		projection.WithPollInterval(pollInterval),
	)
	require.NoError(t, err)

	go group.Run(ctx)

	_, err = log.AppendEvents(
		ctx,
		"account-1",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("account.opened", []byte(`{}`))},
	)
	require.NoError(t, err)

	_, err = log.AppendEvents(
		ctx,
		"order-1",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("order.placed", []byte(`{}`))},
	)
	require.NoError(t, err)

	_, err = log.AppendEvents(
		ctx,
		"account-1",
		version.CheckExact(1),
		event.RawEvents{event.NewRaw("account.closed", []byte(`{}`))},
	)
	require.NoError(t, err)

	// Assert: Wait for the projection to handle the events.
	var received []*event.GlobalRecord
	for range 2 {
		select {
		case rec := <-handledEvents:
			received = append(received, rec)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for events to be handled")
		}
	}

	// Verify the correct events were handled.
	require.Len(t, received, 2)
	assert.Equal(t, version.Version(1), received[0].GlobalVersion()) // Event 1
	assert.Equal(t, "account.opened", received[0].EventName())
	assert.Equal(t, version.Version(3), received[1].GlobalVersion()) // Event 3
	assert.Equal(t, "account.closed", received[1].EventName())

	// The checkpoint should advance to the version of the LATEST event seen,
	// even the ones it didn't handle. This is a key behavior.
	expectedCheckpoint := version.Version(3)
	require.Eventually(t, func() bool {
		checkpointer.mu.Lock()
		defer checkpointer.mu.Unlock()
		return checkpointer.lastSaved["accounts"] == expectedCheckpoint
	}, 1*time.Second, pollInterval, "checkpoint should have been saved at version %d", expectedCheckpoint)

	// Shutdown and verify a clean exit.
	cancel()
	err = group.Wait()

	require.NoError(t, err, "group.Wait() should return no error on clean shutdown")
	require.Len(t, accountProjection.HandleCalls(), 2)
}

func TestNewGroup_InvalidPattern(t *testing.T) {
	log := eventlog.NewMemory()
	checkpointer := newMemoryCheckpointer()

	badProjection := &ProjectionMock{
		NameFunc:       func() string { return "bad-pattern-proj" },
		EventNamesFunc: func() []string { return []string{"account.["} }, // Invalid glob pattern
	}

	_, err := projection.NewGroup(log, checkpointer, []projection.Projection{badProjection})
	require.Error(t, err)
	assert.Contains(
		t,
		err.Error(),
		`projection "bad-pattern-proj" has an invalid event name pattern "account.["`,
	)

	require.ErrorIs(t, err, projection.ErrBadPattern)
}

func TestProjection_MultipleMatchingPatterns(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	log := eventlog.NewMemory()
	checkpointer := newMemoryCheckpointer()
	pollInterval := 50 * time.Millisecond

	handledEvents := make(chan *event.GlobalRecord, 10)

	// This projection listens to two distinct glob patterns.
	multiPatternProjection := &ProjectionMock{
		NameFunc: func() string { return "multi-pattern" },
		EventNamesFunc: func() []string {
			return []string{"account.*", "shipping.initiated"}
		},
		HandleFunc: func(_ context.Context, rec *event.GlobalRecord) error {
			handledEvents <- rec
			return nil
		},
	}

	group, err := projection.NewGroup(
		log,
		checkpointer,
		[]projection.Projection{multiPatternProjection},
		projection.WithPollInterval(pollInterval),
	)
	require.NoError(t, err)

	go group.Run(ctx)

	// Append a series of events, some matching the patterns, some not.
	// 1. Matches "account.*"
	_, err = log.AppendEvents(
		ctx,
		"account-1",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("account.opened", []byte(`{}`))},
	)
	require.NoError(t, err)

	// 2. Does NOT match
	_, err = log.AppendEvents(
		ctx,
		"order-1",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("order.placed", []byte(`{}`))},
	)
	require.NoError(t, err)

	// 3. Matches "shipping.initiated"
	_, err = log.AppendEvents(
		ctx,
		"shipping-1",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("shipping.initiated", []byte(`{}`))},
	)
	require.NoError(t, err)

	// 4. Matches "account.*"
	_, err = log.AppendEvents(
		ctx,
		"account-1",
		version.CheckExact(1),
		event.RawEvents{event.NewRaw("account.closed", []byte(`{}`))},
	)
	require.NoError(t, err)

	// Assert: Wait for the projection to handle the 3 matching events.
	var received []*event.GlobalRecord
	for range 3 {
		select {
		case rec := <-handledEvents:
			received = append(received, rec)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for events to be handled")
		}
	}

	// Verify the correct events were handled.
	require.Len(t, received, 3)
	assert.ElementsMatch(t, []string{"account.opened", "shipping.initiated", "account.closed"},
		[]string{received[0].EventName(), received[1].EventName(), received[2].EventName()})
	assert.ElementsMatch(t, []version.Version{1, 3, 4},
		[]version.Version{received[0].GlobalVersion(), received[1].GlobalVersion(), received[2].GlobalVersion()})

	// The checkpoint must advance to the version of the LATEST event processed by the
	// projection loop, which includes events it was not interested in.
	expectedCheckpoint := version.Version(4)
	require.Eventually(t, func() bool {
		checkpointer.mu.Lock()
		defer checkpointer.mu.Unlock()
		return checkpointer.lastSaved["multi-pattern"] == expectedCheckpoint
	}, 1*time.Second, pollInterval, "checkpoint should have been saved at version %d", expectedCheckpoint)

	// Shutdown and verify a clean exit.
	cancel()
	err = group.Wait()
	require.NoError(t, err, "group.Wait() should return no error on clean shutdown")
	require.Len(t, multiPatternProjection.HandleCalls(), 3)
}
