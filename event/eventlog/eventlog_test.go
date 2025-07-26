package eventlog_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/event/eventlog"

	"github.com/DeluxeOwl/chronicle/version"
)

func collectRecords(t *testing.T, records event.Records) []*event.Record {
	t.Helper()
	var collected []*event.Record
	records(func(r *event.Record, err error) bool {
		require.NoError(t, err)
		collected = append(collected, r)
		return true
	})
	return collected
}

// Test_AppendAndReadEvents_Successful tests the happy path of appending events
// and reading them back, ensuring versioning is handled correctly across multiple appends.
func Test_AppendAndReadEvents_Successful(t *testing.T) {
	store := eventlog.NewMemory()
	logID := event.LogID("stream-1")
	ctx := t.Context()

	rawEvents1 := event.RawEvents{
		event.NewRaw("event-a", []byte(`{"a": 1}`)),
		event.NewRaw("event-b", []byte(`{"b": 2}`)),
	}
	v1, err := store.AppendEvents(ctx, logID, version.CheckAny{}, rawEvents1)
	require.NoError(t, err)
	require.Equal(t, version.Version(2), v1)

	readEvents1 := collectRecords(t, store.ReadEvents(ctx, logID, version.SelectFromBeginning))
	require.Len(t, readEvents1, 2)
	require.Equal(t, "event-a", readEvents1[0].EventName())
	require.Equal(t, version.Version(1), readEvents1[0].Version())
	require.Equal(t, "event-b", readEvents1[1].EventName())
	require.Equal(t, version.Version(2), readEvents1[1].Version())

	rawEvents2 := event.RawEvents{
		event.NewRaw("event-c", []byte(`{"c": 3}`)),
	}
	v2, err := store.AppendEvents(ctx, logID, version.CheckExact(v1), rawEvents2)
	require.NoError(t, err)
	require.Equal(t, version.Version(3), v2)

	// Read all events back and verify the complete log
	allEvents := collectRecords(t, store.ReadEvents(ctx, logID, version.SelectFromBeginning))
	require.Len(t, allEvents, 3)
	require.Equal(t, version.Version(3), allEvents[2].Version())
	require.Equal(t, "event-c", allEvents[2].EventName())
}

// Test_AppendEvents_VersionConflict ensures that the store correctly detects
// and reports a version conflict (transactional guarantee).
func Test_AppendEvents_VersionConflict(t *testing.T) {
	store := eventlog.NewMemory()
	logID := event.LogID("stream-conflict")
	ctx := t.Context()

	// Append one event, log version is now 1
	_, err := store.AppendEvents(
		ctx,
		logID,
		version.CheckAny{},
		event.RawEvents{event.NewRaw("event-1", nil)},
	)
	require.NoError(t, err)

	// Try to append another event with an incorrect expected version (0 instead of 1)
	rawEvents := event.RawEvents{event.NewRaw("event-2", nil)}
	_, err = store.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents)

	require.Error(t, err)
	require.EqualError(
		t,
		err,
		"append events: version conflict error: expected log version: 0, actual: 1",
	)

	var conflictErr *version.ConflictError
	require.ErrorAs(t, err, &conflictErr)

	// Verify the event was not actually appended
	records := collectRecords(t, store.ReadEvents(ctx, logID, version.SelectFromBeginning))
	require.Len(t, records, 1)
}

// Test_ReadEvents_WithSelector tests reading a specific slice of events
// from the log using a version selector.
func Test_ReadEvents_WithSelector(t *testing.T) {
	store := eventlog.NewMemory()
	logID := event.LogID("stream-selector")
	ctx := t.Context()

	// Append 5 events
	var rawEvents event.RawEvents
	for i := 1; i <= 5; i++ {
		rawEvents = append(rawEvents, event.NewRaw(fmt.Sprintf("event-%d", i), nil))
	}
	_, err := store.AppendEvents(ctx, logID, version.CheckAny{}, rawEvents)
	require.NoError(t, err)

	// Read events starting from version 3
	selector := version.Selector{From: 3}
	records := collectRecords(t, store.ReadEvents(ctx, logID, selector))

	// Verify that only events with version >= 3 are returned
	require.Len(t, records, 3)
	require.Equal(t, version.Version(3), records[0].Version())
	require.Equal(t, "event-3", records[0].EventName())
	require.Equal(t, version.Version(4), records[1].Version())
	require.Equal(t, "event-4", records[1].EventName())
	require.Equal(t, version.Version(5), records[2].Version())
	require.Equal(t, "event-5", records[2].EventName())
}

// Test_AppendEvents_ContextCancellation verifies that AppendEvents respects
// context cancellation and aborts the operation.
func Test_AppendEvents_ContextCancellation(t *testing.T) {
	store := eventlog.NewMemory()
	logID := event.LogID("stream-cancel")
	ctx, cancel := context.WithCancel(t.Context())

	cancel() // cancel the context

	rawEvents := event.RawEvents{event.NewRaw("event-1", nil)}
	_, err := store.AppendEvents(ctx, logID, version.CheckAny{}, rawEvents)

	require.Error(t, err)
	require.ErrorContains(t, err, context.Canceled.Error())
}

// Test_Concurrency tests that the memory store can be safely used by multiple
// goroutines concurrently, appending to the same log without data loss.
func Test_Concurrency(t *testing.T) {
	store := eventlog.NewMemory()
	logID := event.LogID("stream-concurrent")
	ctx := t.Context()

	numGoroutines := 10
	eventsPerGoroutine := 10
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := range numGoroutines {
		go func(gID int) {
			defer wg.Done()
			for j := range eventsPerGoroutine {
				// In a real app, you'd fetch the current version first.
				// Here, we simulate a simple retry loop on conflict.
				for {
					currentVersion, err := store.AppendEvents(
						ctx,
						logID,
						version.CheckAny{},
						event.RawEvents{},
					)
					assert.NoError(t, err)

					eventName := fmt.Sprintf("event-g%d-e%d", gID, j)
					rawEvents := event.RawEvents{event.NewRaw(eventName, nil)}

					_, err = store.AppendEvents(
						ctx,
						logID,
						version.CheckExact(currentVersion),
						rawEvents,
					)
					if err == nil {
						break // Success
					}
					// Retry on conflict
					assert.ErrorAs(t, err, new(*version.ConflictError))
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify the final state
	totalEvents := numGoroutines * eventsPerGoroutine
	records := collectRecords(t, store.ReadEvents(ctx, logID, version.SelectFromBeginning))
	require.Len(t, records, totalEvents)

	finalVersion, err := store.AppendEvents(ctx, logID, version.CheckAny{}, event.RawEvents{})
	require.NoError(t, err)
	require.Equal(t, version.Version(totalEvents), finalVersion)
}
