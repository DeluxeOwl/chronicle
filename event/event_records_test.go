package event_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

func TestRecord_Getters(t *testing.T) {
	t.Parallel()
	logID := event.LogID("log-123")
	ver := version.Version(42)
	eventName := "test-event"
	eventData := []byte("test-data")

	rec := event.NewRecord(ver, logID, eventName, eventData)

	require.Equal(t, logID, rec.LogID())
	require.Equal(t, ver, rec.Version())
	require.Equal(t, eventName, rec.EventName())
	require.Equal(t, eventData, rec.Data())
}

func TestRawEvents_ToRecords(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name            string
		rawEvents       event.RawEvents
		logID           event.LogID
		startingVersion version.Version
		wantRecords     []*event.Record
	}{
		{
			name:            "empty slice should produce empty, non-nil record slice",
			rawEvents:       event.RawEvents{},
			logID:           "log-1",
			startingVersion: 10,
			wantRecords:     []*event.Record{},
		},
		{
			name: "multiple events should have correctly incremented versions",
			rawEvents: event.RawEvents{
				event.NewRaw("event-1", []byte("data-1")),
				event.NewRaw("event-2", []byte("data-2")),
			},
			logID:           "log-2",
			startingVersion: 100,
			wantRecords: []*event.Record{
				event.NewRecord(101, "log-2", "event-1", []byte("data-1")),
				event.NewRecord(102, "log-2", "event-2", []byte("data-2")),
			},
		},
		{
			name: "single event with zero starting version",
			rawEvents: event.RawEvents{
				event.NewRaw("event-1", []byte("data-1")),
			},
			logID:           "log-3",
			startingVersion: 0,
			wantRecords: []*event.Record{
				event.NewRecord(1, "log-3", "event-1", []byte("data-1")),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotRecords := tc.rawEvents.ToRecords(tc.logID, tc.startingVersion)

			if len(tc.wantRecords) == 0 {
				require.NotNil(t, gotRecords)
				require.Empty(t, gotRecords)
				return
			}

			require.Len(t, gotRecords, len(tc.wantRecords))
			for i, got := range gotRecords {
				want := tc.wantRecords[i]
				require.Equal(t, want.Version(), got.Version(), "Version mismatch at index %d", i)
				require.Equal(t, want.LogID(), got.LogID(), "LogID mismatch at index %d", i)
				require.Equal(
					t,
					want.EventName(),
					got.EventName(),
					"EventName mismatch at index %d",
					i,
				)
				require.Equal(t, want.Data(), got.Data(), "Data mismatch at index %d", i)
			}
		})
	}
}

func TestRawEvents_All(t *testing.T) {
	t.Parallel()

	t.Run("should iterate over all items", func(t *testing.T) {
		t.Parallel()
		rawEvents := event.RawEvents{
			event.NewRaw("event-1", []byte("data-1")),
			event.NewRaw("event-2", []byte("data-2")),
		}

		var collected []event.Raw
		for r := range rawEvents.All() {
			collected = append(collected, r)
		}
		// Raw struct contains a slice, but testify's Equal should handle it.
		require.Equal(t, []event.Raw(rawEvents), collected)
	})

	t.Run("should stop iteration when yield returns false", func(t *testing.T) {
		t.Parallel()
		rawEvents := event.RawEvents{
			event.NewRaw("event-1", []byte("data-1")),
			event.NewRaw("event-2", []byte("data-2")),
		}

		var count int
		seq := rawEvents.All()
		// Manually call the iterator function to control the yield.
		seq(func(r event.Raw) bool {
			count++
			return false // Stop after the first item.
		})
		require.Equal(t, 1, count)
	})

	t.Run("should handle an empty slice", func(t *testing.T) {
		t.Parallel()
		var rawEvents event.RawEvents
		var count int
		for range rawEvents.All() {
			count++
		}
		require.Equal(t, 0, count)
	})
}
