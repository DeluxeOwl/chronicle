package projection_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
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
	mu            sync.Mutex
	versions      map[string]version.Version
	savedVersions map[string][]version.Version // Record all saves
	saveCalls     map[string]int
	getCalls      map[string]int
}

//nolint:exhaustruct // not needed.
func newMemoryCheckpointer() *memoryCheckpointer {
	return &memoryCheckpointer{
		versions:      make(map[string]version.Version),
		savedVersions: make(map[string][]version.Version),
		saveCalls:     make(map[string]int),
		getCalls:      make(map[string]int),
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
	m.savedVersions[projectionName] = append(m.savedVersions[projectionName], v)
	return nil
}

// GetSavedVersions is a helper for tests to inspect the history of saves.
func (m *memoryCheckpointer) GetSavedVersions(projectionName string) []version.Version {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy to avoid race conditions
	result := make([]version.Version, len(m.savedVersions[projectionName]))
	copy(result, m.savedVersions[projectionName])
	return result
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
		saved := checkpointer.GetSavedVersions("accounts")
		// For the default policy, we expect exactly one save at the end of the batch
		return len(saved) == 1 && saved[0] == expectedCheckpoint
	}, 1*time.Second, pollInterval, "checkpoint should have been saved at version %d", expectedCheckpoint)

	// Shutdown and verify a clean exit.
	cancel()

	require.NoError(t, err)
	require.Len(t, accountProjection.HandleCalls(), 2)
}

func TestProjectionRunner_StopsOnHandleError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	log := eventlog.NewMemory()
	checkpointer := newMemoryCheckpointer()
	pollInterval := 50 * time.Millisecond
	handleErr := errors.New("handler failed catastrophically")

	proj := &AsyncProjectionMock{
		NameFunc: func() string { return "failing-proj" },
		MatchesEventFunc: func(eventName string) bool {
			return strings.HasPrefix(eventName, "account.")
		},
		HandleFunc: func(_ context.Context, rec *event.GlobalRecord) error {
			// This projection fails when it sees a "account.critical.failure" event
			if rec.EventName() == "account.critical.failure" {
				return handleErr
			}
			return nil
		},
	}

	runner, err := projection.NewAsyncProjectionRunner(
		log,
		checkpointer,
		proj,
		projection.WithPollInterval(pollInterval),
		//nolint:exhaustruct // not needed
		projection.WithSlogHandler(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})),
	)
	require.NoError(t, err)

	runnerErrChan := make(chan error, 1)
	go func() {
		runnerErrChan <- runner.Run(ctx)
	}()

	// Append a successful event first
	_, err = log.AppendEvents(
		ctx,
		"account-1",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("account.opened", nil)},
	)
	require.NoError(t, err)

	// Wait for the first checkpoint to be saved successfully
	require.Eventually(t, func() bool {
		cp, _ := checkpointer.GetCheckpoint(ctx, "failing-proj")
		return cp == 1
	}, time.Second, pollInterval, "checkpoint for first event was not saved")

	// Append the event that will cause the handler to fail
	_, err = log.AppendEvents(
		ctx,
		"account-2",
		version.CheckExact(0),
		event.RawEvents{event.NewRaw("account.critical.failure", nil)},
	)
	require.NoError(t, err)

	// The runner should stop and return the handler's error, wrapped
	select {
	case err := <-runnerErrChan:
		require.Error(t, err)
		require.ErrorIs(t, err, handleErr, "the original error should be wrapped")
		require.Contains(t, err.Error(), `projection "failing-proj": handler failed on event 2`)
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not stop after handler error")
	}

	// Verify Handle was called for both matching events
	calls := proj.HandleCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, "account.opened", calls[0].Rec.EventName())
	assert.Equal(t, "account.critical.failure", calls[1].Rec.EventName())

	// Verify the checkpoint was not advanced past the last successful batch
	finalCheckpoint, err := checkpointer.GetCheckpoint(ctx, "failing-proj")
	require.NoError(t, err)
	assert.Equal(
		t,
		version.Version(1),
		finalCheckpoint,
		"checkpoint should not advance after a handler failure",
	)
}

func TestProjectionRunner_CheckpointPolicies(t *testing.T) {
	t.Parallel()
	projName := "policy-proj"
	pollInterval := 10 * time.Millisecond
	waitTimeout := 2 * time.Second

	// Create 5 raw events to be used in the tests
	var rawEvents event.RawEvents
	for range 5 {
		rawEvents = append(rawEvents, event.NewRaw("event.test", nil))
	}

	// This subtest verifies the default behavior: checkpointing only at the end of a batch.
	t.Run("Default Policy - Checkpoints at end of batch", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		log := eventlog.NewMemory()
		checkpointer := newMemoryCheckpointer()
		projMock := &AsyncProjectionMock{
			NameFunc:         func() string { return projName },
			MatchesEventFunc: func(eventName string) bool { return true },
			HandleFunc:       func(ctx context.Context, rec *event.GlobalRecord) error { return nil },
		}

		runner, err := projection.NewAsyncProjectionRunner(log, checkpointer, projMock,
			projection.WithPollInterval(pollInterval),
		)
		require.NoError(t, err)

		go runner.Run(ctx)

		// Append 5 events
		_, err = log.AppendEvents(ctx, "stream-1", version.CheckExact(0), rawEvents)
		require.NoError(t, err)

		// We expect only one save at the very end of the batch
		expectedVersions := []version.Version{5}
		require.Eventually(t, func() bool {
			return len(projMock.HandleCalls()) == 5
		}, waitTimeout, pollInterval, "expected all events to be handled")

		// Assert that the checkpoint was saved once with the final version
		saved := checkpointer.GetSavedVersions(projName)
		assert.Equal(t, expectedVersions, saved)
	})

	// This subtest verifies the EveryNEvents policy.
	t.Run("EveryNEvents Policy - Checkpoints every N events", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		log := eventlog.NewMemory()
		checkpointer := newMemoryCheckpointer()
		projMock := &AsyncProjectionMock{
			NameFunc:         func() string { return projName },
			MatchesEventFunc: func(eventName string) bool { return true },
			HandleFunc:       func(ctx context.Context, rec *event.GlobalRecord) error { return nil },
		}

		policy := projection.EveryNEvents(2)
		runner, err := projection.NewAsyncProjectionRunner(log, checkpointer, projMock,
			projection.WithPollInterval(pollInterval),
			projection.WithCheckpointPolicy(policy),
		)
		require.NoError(t, err)

		go runner.Run(ctx)

		// Append 5 events
		_, err = log.AppendEvents(ctx, "stream-1", version.CheckExact(0), rawEvents)
		require.NoError(t, err)

		// We expect checkpoints after event 2, event 4, and then for the rest of the batch (event 5)
		expectedVersions := []version.Version{2, 4, 5}

		// Wait until all saves have occurred
		require.Eventually(t, func() bool {
			return len(checkpointer.GetSavedVersions(projName)) == len(expectedVersions)
		}, waitTimeout, pollInterval, "expected multiple checkpoints to be saved")

		saved := checkpointer.GetSavedVersions(projName)
		assert.Equal(t, expectedVersions, saved)
		assert.Len(t, projMock.HandleCalls(), 5, "expected all 5 events to be handled")
	})

	// This subtest verifies the composite policy triggers on either condition.
	t.Run("AnyOf Policy - Checkpoints on N events OR duration", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)

		log := eventlog.NewMemory()
		checkpointer := newMemoryCheckpointer()

		// The handler will sleep, allowing the duration policy to trigger
		handlerSleep := 60 * time.Millisecond
		projMock := &AsyncProjectionMock{
			NameFunc:         func() string { return projName },
			MatchesEventFunc: func(eventName string) bool { return true },
			HandleFunc: func(ctx context.Context, rec *event.GlobalRecord) error {
				time.Sleep(handlerSleep)
				return nil
			},
		}

		// Policy: Checkpoint every 3 events OR after 100ms.
		// Since each event takes 60ms, the 100ms duration will trigger after the 2nd event.
		policy := projection.AnyOf(
			projection.EveryNEvents(3),
			projection.AfterDuration(100*time.Millisecond),
		)
		runner, err := projection.NewAsyncProjectionRunner(log, checkpointer, projMock,
			projection.WithPollInterval(pollInterval),
			projection.WithCheckpointPolicy(policy),
		)
		require.NoError(t, err)

		go runner.Run(ctx)

		// Append 5 events
		_, err = log.AppendEvents(ctx, "stream-1", version.CheckExact(0), rawEvents)
		require.NoError(t, err)

		// TRACE:
		// - Event 1 handled (~60ms) -> No trigger
		// - Event 2 handled (~120ms total) -> Time trigger! Save checkpoint @ version 2. Reset counters.
		// - Event 3 handled (~60ms since last save) -> No trigger
		// - Event 4 handled (~120ms since last save) -> Time trigger! Save checkpoint @ version 4. Reset.
		// - Event 5 handled (~60ms since last save) -> No trigger.
		// - End of batch -> Final checkpoint for remaining event. Save @ version 5.
		expectedVersions := []version.Version{2, 4, 5}

		require.Eventually(t, func() bool {
			return len(checkpointer.GetSavedVersions(projName)) == len(expectedVersions)
		}, waitTimeout, pollInterval, "expected checkpoints based on composite policy")

		saved := checkpointer.GetSavedVersions(projName)
		assert.Equal(t, expectedVersions, saved)
		assert.Len(t, projMock.HandleCalls(), 5, "expected all 5 events to be handled")
	})
}
