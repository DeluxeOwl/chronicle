package eventlog_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/internal/testutils"

	"github.com/DeluxeOwl/chronicle/version"
	_ "github.com/jackc/pgx/v5/stdlib" // Import the pgx stdlib driver
	_ "github.com/mattn/go-sqlite3"
)

// Test_AppendAndReadEvents_Successful tests the happy path of appending events
// and reading them back, ensuring versioning is handled correctly across multiple appends.
func Test_AppendAndReadEvents_Successful(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			logID := event.LogID("stream-1")
			ctx := t.Context()

			rawEvents1 := event.RawEvents{
				event.NewRaw("event-a", []byte(`{"a": 1}`)),
				event.NewRaw("event-b", []byte(`{"b": 2}`)),
			}
			v1, err := el.Log.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents1)
			require.NoError(t, err)
			require.Equal(t, version.Version(2), v1)

			readEvents1 := testutils.CollectRecords(
				t,
				el.Log.ReadEvents(ctx, logID, version.SelectFromBeginning),
			)
			require.Len(t, readEvents1, 2)
			require.Equal(t, "event-a", readEvents1[0].EventName())
			require.Equal(t, version.Version(1), readEvents1[0].Version())
			require.Equal(t, "event-b", readEvents1[1].EventName())
			require.Equal(t, version.Version(2), readEvents1[1].Version())

			rawEvents2 := event.RawEvents{
				event.NewRaw("event-c", []byte(`{"c": 3}`)),
			}
			v2, err := el.Log.AppendEvents(ctx, logID, version.CheckExact(v1), rawEvents2)
			require.NoError(t, err)
			require.Equal(t, version.Version(3), v2)

			// Read all events back and verify the complete log
			allEvents := testutils.CollectRecords(
				t,
				el.Log.ReadEvents(ctx, logID, version.SelectFromBeginning),
			)
			require.Len(t, allEvents, 3)
			require.Equal(t, version.Version(3), allEvents[2].Version())
			require.Equal(t, "event-c", allEvents[2].EventName())
		})
	}
}

// Test_AppendEvents_VersionConflict ensures that the store correctly detects
// and reports a version conflict (transactional guarantee).
func Test_AppendEvents_VersionConflict(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			logID := event.LogID("stream-conflict")
			ctx := t.Context()

			// Append one event, log version is now 1
			_, err := el.Log.AppendEvents(
				ctx,
				logID,
				version.CheckExact(0),
				event.RawEvents{event.NewRaw("event-1", nil)},
			)
			require.NoError(t, err)

			// Try to append another event with an incorrect expected version (0 instead of 1)
			rawEvents := event.RawEvents{event.NewRaw("event-2", nil)}
			_, err = el.Log.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents)
			require.Error(t, err)

			var conflictErr *version.ConflictError
			require.ErrorAs(t, err, &conflictErr)

			// Verify the event was not actually appended
			records := testutils.CollectRecords(
				t,
				el.Log.ReadEvents(ctx, logID, version.SelectFromBeginning),
			)
			require.Len(t, records, 1)
		})
	}
}

// Test_ReadEvents_WithSelector tests reading a specific slice of events
// from the log using a version selector.
func Test_ReadEvents_WithSelector(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			logID := event.LogID("stream-selector")
			ctx := t.Context()

			// Append 5 events
			var rawEvents event.RawEvents
			for i := 1; i <= 5; i++ {
				rawEvents = append(rawEvents, event.NewRaw(fmt.Sprintf("event-%d", i), nil))
			}
			_, err := el.Log.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents)
			require.NoError(t, err)

			// Read events starting from version 3
			selector := version.Selector{From: 3}
			records := testutils.CollectRecords(t, el.Log.ReadEvents(ctx, logID, selector))

			// Verify that only events with version >= 3 are returned
			require.Len(t, records, 3)
			require.Equal(t, version.Version(3), records[0].Version())
			require.Equal(t, "event-3", records[0].EventName())
			require.Equal(t, version.Version(4), records[1].Version())
			require.Equal(t, "event-4", records[1].EventName())
			require.Equal(t, version.Version(5), records[2].Version())
			require.Equal(t, "event-5", records[2].EventName())
		})
	}
}

// Test_AppendEvents_Concurrency tests that the event log can handle concurrent
// append operations from multiple clients, ensuring that all events are written
// correctly and versioning is maintained without race conditions.
//
//nolint:gocognit
func Test_AppendEvents_Concurrency(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			const (
				numGoroutines      = 10
				eventsPerGoroutine = 5
				totalEvents        = numGoroutines * eventsPerGoroutine
			)

			logID := event.LogID("concurrent-stream")
			ctx := t.Context()
			var wg sync.WaitGroup
			wg.Add(numGoroutines)

			for i := range numGoroutines {
				go func(gID int) {
					defer wg.Done()

					// Each goroutine will try to append its batch of events.
					// It will retry on version conflicts.
					rawEvents := event.RawEvents{}
					for j := range eventsPerGoroutine {
						eventName := fmt.Sprintf("event-g%d-e%d", gID, j)
						rawEvents = append(rawEvents, event.NewRaw(eventName, nil))
					}

					// Start with an initial guess for the version.
					// On conflict, this will be updated to the actual version from the error.
					lastKnownVersion := version.Version(0)

					for range 20 { // Limit retries to avoid infinite loops
						_, err := el.Log.AppendEvents(
							ctx,
							logID,
							version.CheckExact(lastKnownVersion),
							rawEvents,
						)
						if err == nil {
							return // Success
						}

						var conflictErr *version.ConflictError
						if errors.As(err, &conflictErr) {
							// Another goroutine succeeded. Update our version and retry.
							lastKnownVersion = conflictErr.Actual
							continue
						}

						// Any other error is unexpected and should fail the test.
						assert.NoError(t, err, "unexpected error during concurrent append")
						return
					}
					assert.Fail(t, "goroutine failed to append events after multiple retries")
				}(i)
			}

			wg.Wait()

			// Verification
			records := testutils.CollectRecords(
				t,
				el.Log.ReadEvents(ctx, logID, version.SelectFromBeginning),
			)
			require.Len(t, records, totalEvents, "incorrect number of total events written")

			// Check for sequential versions
			versions := make(map[version.Version]bool)
			for _, r := range records {
				versions[r.Version()] = true
			}
			require.Len(t, versions, totalEvents, "duplicate or missing versions found")

			for i := 1; i <= totalEvents; i++ {
				_, ok := versions[version.Version(i)]
				require.True(t, ok, "missing version %d", i)
			}
		})
	}
}

// Test_AppendEvents_ContextCancellation verifies that AppendEvents respects
// context cancellation and aborts the operation.
func Test_AppendEvents_ContextCancellation(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			logID := event.LogID("stream-cancel")
			ctx, cancel := context.WithCancel(t.Context())

			cancel() // cancel the context

			rawEvents := event.RawEvents{event.NewRaw("event-1", nil)}
			_, err := el.Log.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents)

			require.Error(t, err)
			require.ErrorContains(t, err, context.Canceled.Error())
		})
	}
}

func Test_SqliteSyncProjection(t *testing.T) {
	t.Run("without errors", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "sqlite-*.db")
		require.NoError(t, err)

		sqliteDB, err := sql.Open("sqlite3", f.Name())
		require.NoError(t, err)
		defer sqliteDB.Close()

		sqliteLog, err := eventlog.NewSqlite(sqliteDB)
		require.NoError(t, err)

		sqliteProcessor := &SyncProjectionMock[*sql.Tx]{
			MatchesEventFunc: func(eventName string) bool {
				return true
			},
			HandleFunc: func(ctx context.Context, tx *sql.Tx, records []*event.Record) error {
				return nil
			},
		}

		log := event.NewLogWithProjection(sqliteLog, sqliteProcessor)
		require.NotNil(t, log)

		rawEvents1 := event.RawEvents{
			event.NewRaw("event-a", []byte(`{"a": 1}`)),
			event.NewRaw("event-b", []byte(`{"b": 2}`)),
		}
		v1, err := log.AppendEvents(
			t.Context(),
			event.LogID("123"),
			version.CheckExact(0),
			rawEvents1,
		)
		require.NoError(t, err)
		require.Equal(t, version.Version(2), v1)

		require.Len(t, sqliteProcessor.calls.Handle, 1)
	})

	t.Run("with errors", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "sqlite-*.db")
		require.NoError(t, err)

		sqliteDB, err := sql.Open("sqlite3", f.Name())
		require.NoError(t, err)
		defer sqliteDB.Close()

		sqliteLog, err := eventlog.NewSqlite(sqliteDB)
		require.NoError(t, err)

		sqliteProcessor := &SyncProjectionMock[*sql.Tx]{
			MatchesEventFunc: func(eventName string) bool {
				return true
			},
			HandleFunc: func(ctx context.Context, tx *sql.Tx, records []*event.Record) error {
				return errors.New("error")
			},
		}

		log := event.NewLogWithProjection(sqliteLog, sqliteProcessor)
		require.NotNil(t, log)

		rawEvents1 := event.RawEvents{
			event.NewRaw("event-a", []byte(`{"a": 1}`)),
			event.NewRaw("event-b", []byte(`{"b": 2}`)),
		}

		v1, err := log.AppendEvents(
			t.Context(),
			event.LogID("123"),
			version.CheckExact(0),
			rawEvents1,
		)
		require.Error(t, err)
		require.Equal(t, version.Version(0), v1)

		require.Len(t, sqliteProcessor.calls.Handle, 1)

		records, err := log.ReadEvents(t.Context(), event.LogID("123"), version.SelectFromBeginning).
			Collect()
		require.NoError(t, err)
		require.Empty(t, records)
	})
}

func Test_ReadAllEvents(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupGlobalEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			for i := range 10 {
				eventToAppend := event.NewRaw(
					fmt.Sprintf("foo-event-%d", i),
					[]byte("{\"data\":\"value\"}"),
				)
				_, err := el.Log.AppendEvents(
					t.Context(),
					event.LogID("foo/123"),
					version.CheckExact(i),
					event.RawEvents{eventToAppend},
				)
				require.NoError(t, err)
			}
			for i := range 10 {
				eventToAppend := event.NewRaw(
					fmt.Sprintf("bar-event-%d", i),
					[]byte("{\"data\":\"value\"}"),
				)
				_, err := el.Log.AppendEvents(
					t.Context(),
					event.LogID("bar/123"),
					version.CheckExact(i),
					event.RawEvents{eventToAppend},
				)
				require.NoError(t, err)
			}

			versionToCheck := version.Zero + 1
			for ev, err := range el.Log.ReadAllEvents(t.Context(), version.SelectFromBeginning) {
				require.NoError(t, err)
				require.Equal(t, versionToCheck, ev.GlobalVersion())
				versionToCheck++
			}
		})
	}
}

// Test_ReadEvents_WithSelectorTo tests reading a specific slice of events
// from the log using a version selector that specifies a 'To' version.
func Test_ReadEvents_WithSelectorTo(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			logID := event.LogID("stream-selector-to")
			ctx := t.Context()

			// Append 10 events to create a stream from version 1 to 10
			var rawEvents event.RawEvents
			for i := 1; i <= 10; i++ {
				rawEvents = append(rawEvents, event.NewRaw(fmt.Sprintf("event-%d", i), nil))
			}
			_, err := el.Log.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents)
			require.NoError(t, err)

			//nolint:exhaustruct // Unnecessary.
			testCases := []struct {
				name                 string
				selector             version.Selector
				expectedLen          int
				expectedFirstVersion version.Version
				expectedLastVersion  version.Version
			}{
				{
					name:                 "select a slice from the middle",
					selector:             version.Selector{From: 3, To: 7},
					expectedLen:          5,
					expectedFirstVersion: 3,
					expectedLastVersion:  7,
				},
				{
					name:                 "select a slice until the end",
					selector:             version.Selector{From: 8, To: 10},
					expectedLen:          3,
					expectedFirstVersion: 8,
					expectedLastVersion:  10,
				},
				{
					name:                 "select a single event",
					selector:             version.Selector{From: 5, To: 5},
					expectedLen:          1,
					expectedFirstVersion: 5,
					expectedLastVersion:  5,
				},
				{
					name:                 "select with 'To' beyond the last event",
					selector:             version.Selector{From: 8, To: 15},
					expectedLen:          3, // returns up to the last available event
					expectedFirstVersion: 8,
					expectedLastVersion:  10,
				},
				{
					name:                 "select with unbounded 'To' (To=0)",
					selector:             version.Selector{From: 9, To: 0},
					expectedLen:          2, // should behave like no 'To' was specified
					expectedFirstVersion: 9,
					expectedLastVersion:  10,
				},
				{
					name:        "select with 'From' beyond last event",
					selector:    version.Selector{From: 11, To: 15},
					expectedLen: 0,
				},
				{
					name:        "select with 'To' before 'From'",
					selector:    version.Selector{From: 5, To: 4},
					expectedLen: 0,
				},
			}

			for _, tc := range testCases {
				t.Run(tc.name, func(t *testing.T) {
					records := testutils.CollectRecords(
						t,
						el.Log.ReadEvents(ctx, logID, tc.selector),
					)

					require.Len(t, records, tc.expectedLen)

					if tc.expectedLen > 0 {
						assert.Equal(t, tc.expectedFirstVersion, records[0].Version())
						assert.Equal(t, tc.expectedLastVersion, records[len(records)-1].Version())
					}
				})
			}
		})
	}
}

// Test_ReadAllEvents_WithSelectorTo tests reading a specific slice from the global
// event stream using a selector that specifies a 'To' global version.
func Test_ReadAllEvents_WithSelectorTo(t *testing.T) {
	eventLogs, closeDBs := testutils.SetupGlobalEventLogs(t)
	defer closeDBs()

	for _, el := range eventLogs {
		t.Run(el.Name, func(t *testing.T) {
			ctx := t.Context()
			totalEvents := 20

			// Append 20 events across two different streams to create a global log
			for i := range 10 {
				_, err := el.Log.AppendEvents(
					ctx, event.LogID("stream-a"), version.CheckExact(i),
					event.RawEvents{event.NewRaw("event-a", nil)},
				)
				require.NoError(t, err)
			}
			for i := range 10 {
				_, err := el.Log.AppendEvents(
					ctx, event.LogID("stream-b"), version.CheckExact(i),
					event.RawEvents{event.NewRaw("event-b", nil)},
				)
				require.NoError(t, err)
			}

			//nolint:exhaustruct // Unnecessary.
			testCases := []struct {
				name                 string
				selector             version.Selector
				expectedLen          int
				expectedFirstVersion version.Version
				expectedLastVersion  version.Version
			}{
				{
					name:                 "select a slice from the middle",
					selector:             version.Selector{From: 5, To: 15},
					expectedLen:          11,
					expectedFirstVersion: 5,
					expectedLastVersion:  15,
				},
				{
					name:                 "select a slice until the end",
					selector:             version.Selector{From: 18, To: 20},
					expectedLen:          3,
					expectedFirstVersion: 18,
					expectedLastVersion:  20,
				},
				{
					name:                 "select a single event",
					selector:             version.Selector{From: 10, To: 10},
					expectedLen:          1,
					expectedFirstVersion: 10,
					expectedLastVersion:  10,
				},
				{
					name:                 "select with 'To' beyond the last event",
					selector:             version.Selector{From: 15, To: 30},
					expectedLen:          6, // returns up to the last available event (20)
					expectedFirstVersion: 15,
					expectedLastVersion:  version.Version(totalEvents),
				},
				{
					name:                 "select with unbounded 'To' (To=0)",
					selector:             version.Selector{From: 18, To: 0},
					expectedLen:          3, // should behave like no 'To' was specified
					expectedFirstVersion: 18,
					expectedLastVersion:  version.Version(totalEvents),
				},
				{
					name:        "select with 'From' beyond last event",
					selector:    version.Selector{From: 21, To: 30},
					expectedLen: 0,
				},
				{
					name:        "select with 'To' before 'From'",
					selector:    version.Selector{From: 10, To: 9},
					expectedLen: 0,
				},
			}

			for _, tc := range testCases {
				t.Run(tc.name, func(t *testing.T) {
					var records []*event.GlobalRecord
					for r, err := range el.Log.ReadAllEvents(ctx, tc.selector) {
						require.NoError(t, err)
						records = append(records, r)
					}

					require.Len(t, records, tc.expectedLen)

					if tc.expectedLen > 0 {
						assert.Equal(t, tc.expectedFirstVersion, records[0].GlobalVersion())
						assert.Equal(
							t,
							tc.expectedLastVersion,
							records[len(records)-1].GlobalVersion(),
						)
					}
				})
			}
		})
	}
}
