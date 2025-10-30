package projection_test

import (
	"context"
	"strings"
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

func TestProjectionRunner_Run(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	log := eventlog.NewMemory()
	checkpointer := newMemoryCheckpointer()
	pollInterval := 50 * time.Millisecond

	accountProjection := &AsyncProjectionMock{
		NameFunc: func() string { return "accounts" },
		MatchesEventFunc: func(eventName string) bool {
			return strings.HasPrefix(eventName, "account.")
		},
		HandleFunc: func(_ context.Context, rec *event.GlobalRecord) error {
			return nil
		},
	}

	runner, err := projection.NewAsyncProjectionRunner(
		log,
		checkpointer,
		accountProjection,
		projection.WithPollInterval(pollInterval),
	)
	require.NoError(t, err)

	go runner.Run(ctx)

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
	require.Eventually(t, func() bool {
		return len(accountProjection.HandleCalls()) == 2
	}, 2*time.Second, 10*time.Millisecond, "expected 2 events to be handled")

	// Verify the correct events were handled.
	calls := accountProjection.HandleCalls()
	require.Len(t, calls, 2)

	assert.Equal(t, version.Version(1), calls[0].Rec.GlobalVersion()) // Event 1
	assert.Equal(t, "account.opened", calls[0].Rec.EventName())
	assert.Equal(t, version.Version(3), calls[1].Rec.GlobalVersion()) // Event 3
	assert.Equal(t, "account.closed", calls[1].Rec.EventName())

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

	require.NoError(t, err)
	require.Len(t, accountProjection.HandleCalls(), 2)
}
