package eventlog_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"

	"github.com/DeluxeOwl/chronicle/version"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemory_ReadAllEvents_WithTailing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	store := eventlog.NewMemory(eventlog.WithMemoryGlobalTailing())

	var wg sync.WaitGroup

	eventsChan := make(chan *event.GlobalRecord, 10)

	initialEvent := event.NewRaw("initial_event", []byte(`{"id":1}`))
	_, err := store.AppendEvents(ctx, "log-1", version.CheckExact(0), event.RawEvents{initialEvent})
	require.NoError(t, err, "Failed to append initial event")

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(eventsChan)

		t.Log("Reader: Starting to iterate over events...")

		// This loop will block automatically when it runs out of events.
		for record, err := range store.ReadAllEvents(ctx, version.Selector{From: 1}) {
			if err != nil {
				if errors.Is(err, context.Canceled) {
					t.Log("Reader: Context canceled, shutting down gracefully.")
					return
				}

				assert.NoError(t, err, "Iterator returned an unexpected error")
				return
			}

			t.Logf("Reader: Received event with global version %d", record.GlobalVersion())

			eventsChan <- record
		}
	}()

	// Give the reader a moment to start and process the first event.
	time.Sleep(100 * time.Millisecond)

	// Append new events with a delay.
	// The blocked reader should wake up and receive them
	t.Log("Main: Appending tailed event 1...")
	tailedEvents_1 := event.RawEvents{event.NewRaw("tailed_event_1", []byte(`{"id":2}`))}
	_, err = store.AppendEvents(ctx, "log-1", version.CheckExact(1), tailedEvents_1)
	require.NoError(t, err, "Failed to append tailed event 1")

	time.Sleep(600 * time.Millisecond)

	t.Log("Main: Appending tailed event 2...")
	tailedEvents_2 := event.RawEvents{event.NewRaw("tailed_event_2", []byte(`{"id":3}`))}
	_, err = store.AppendEvents(ctx, "log-1", version.CheckExact(2), tailedEvents_2)
	require.NoError(t, err, "Failed to append tailed event 2")

	// Give the reader a moment to process the final event.
	time.Sleep(100 * time.Millisecond)

	t.Log("Main: All events sent. Cancelling context to stop reader.")
	cancel()
	wg.Wait()
	t.Log("Main: Reader has shut down. Verifying results.")

	var receivedEvents []*event.GlobalRecord
	for evt := range eventsChan {
		receivedEvents = append(receivedEvents, evt)
	}

	require.Len(t, receivedEvents, 3, "Expected to receive 3 events")

	expectedNames := []string{"initial_event", "tailed_event_1", "tailed_event_2"}
	for i, name := range expectedNames {
		require.Equal(t, name, receivedEvents[i].EventName(),
			"Event %d: expected name %q", i, name)
		require.Equal(t, version.Version(i+1), receivedEvents[i].GlobalVersion(),
			"Event %d: expected global version %d", i, i+1)
	}

	t.Log("Test finished successfully!")
}
