package eventlog_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
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

func createRawEvents(count int) event.RawEvents {
	events := make(event.RawEvents, count)
	for i := range count {
		events[i] = event.NewRaw(
			fmt.Sprintf("test_event_%d", i),
			[]byte(`{"value":"some-data-for-event"}`),
		)
	}
	return events
}

func createBenchLog(b *testing.B, logType string) event.Log {
	b.Helper()

	switch logType {
	case "memory":
		return eventlog.NewMemory()
	case "sqlite":
		dbPath := filepath.Join(b.TempDir(), "test.db")
		db, err := sql.Open("sqlite3", dbPath)
		require.NoError(b, err)

		b.Cleanup(func() { db.Close() })
		log, err := eventlog.NewSqlite(db)
		require.NoError(b, err)

		return log
	case "postgres":
		db, clean := testutils.SetupPostgres(b)
		b.Cleanup(clean)

		// use a unique table name for each benchmark for isolation
		log, err := eventlog.NewPostgres(db)
		require.NoError(b, err)

		b.Cleanup(func() {
			db.Exec("DROP TABLE IF EXISTS chronicle_events;")
		})

		return log
	default:
		b.Fatalf("unknown log type: %s", logType)
		return nil
	}
}

// populateLog pre-fills an event log with a specified number of events for a single stream.
func populateLog(b *testing.B, log event.Log, logID event.LogID, numEvents int) {
	b.Helper()
	ctx := b.Context()
	batchSize := 100
	events := createRawEvents(batchSize)
	currentVersion := version.Version(0)

	for i := 0; i < numEvents; i += batchSize {
		v, err := log.AppendEvents(ctx, logID, version.CheckExact(currentVersion), events)
		require.NoError(b, err)

		currentVersion = v
	}
}

// Measures the performance of writing events.
func BenchmarkAppendEvents(b *testing.B) {
	scenarios := []struct {
		name      string
		batchSize int
	}{
		{"BatchSize1", 1},
		{"BatchSize10", 10},
		{"BatchSize100", 100},
	}

	logTypes := []string{"memory", "sqlite", "postgres"}

	for _, s := range scenarios {
		b.Run(s.name, func(b *testing.B) {
			rawEvents := createRawEvents(s.batchSize)
			ctx := b.Context()

			for _, logType := range logTypes {
				b.Run(logType, func(b *testing.B) {
					log := createBenchLog(b, logType)

					b.ResetTimer()
					for i := range b.N {
						// Use a unique log ID for each append to test the "first write" performance
						// and ensure the version check is always against version 0.
						logID := event.LogID(fmt.Sprintf("log-%s-%d", s.name, i))
						_, err := log.AppendEvents(ctx, logID, version.CheckExact(0), rawEvents)
						require.NoError(b, err)
					}
				})
			}
		})
	}
}

// Measures the performance of reading an aggregate's event stream.
func BenchmarkReadEvents(b *testing.B) {
	scenarios := []struct {
		name         string
		streamLength int
	}{
		{"StreamLength100", 100},
		{"StreamLength1000", 1000},
	}

	logTypes := []string{"memory", "sqlite", "postgres"}

	for _, s := range scenarios {
		b.Run(s.name, func(b *testing.B) {
			for _, logType := range logTypes {
				b.Run(logType, func(b *testing.B) {
					log := createBenchLog(b, logType)
					logID := event.LogID("test-stream")
					populateLog(b, log, logID, s.streamLength)
					ctx := b.Context()

					b.ResetTimer()
					for b.Loop() {
						records := log.ReadEvents(ctx, logID, version.SelectFromBeginning)

						// We must consume the iterator to get a real measurement.
						_, err := records.Collect()
						require.NoError(b, err)
					}
				})
			}
		})
	}
}

// BenchmarkReadAllEvents measures the performance of reading the global event log.
func BenchmarkReadAllEvents(b *testing.B) {
	scenarios := []struct {
		name            string
		numStreams      int
		eventsPerStream int
	}{
		{"Total1000_Events", 10, 100},
		{"Total10000_Events", 10, 1000},
	}

	logTypes := []string{"memory", "sqlite", "postgres"}

	for _, s := range scenarios {
		b.Run(s.name, func(b *testing.B) {
			for _, logType := range logTypes {
				b.Run(logType, func(b *testing.B) {
					log := createBenchLog(b, logType)
					globalLog, ok := log.(event.GlobalLog)
					if !ok {
						b.Skipf("%s does not implement event.GlobalLog", logType)
					}

					// Populate multiple streams
					for i := range s.numStreams {
						logID := event.LogID(fmt.Sprintf("stream-%d", i))
						populateLog(b, log, logID, s.eventsPerStream)
					}

					ctx := b.Context()
					b.ResetTimer()

					for b.Loop() {
						records := globalLog.ReadAllEvents(ctx, version.Selector{From: 1})
						// Consume iterator
						_, err := records.Collect()
						require.NoError(b, err)
					}
				})
			}
		})
	}
}

func BenchmarkAppendEventsConcurrent(b *testing.B) {
	// This benchmark measures how well each event log handles concurrent writes.
	// It simulates a real-world scenario where many different operations are happening
	// at the same time (e.g., many users interacting with the system).
	const numGoroutines = 100

	logTypes := []string{"memory", "sqlite", "postgres"}
	rawEvents := createRawEvents(5) // Each goroutine will append 5 events.

	for _, logType := range logTypes {
		b.Run(logType, func(b *testing.B) {
			log := createBenchLog(b, logType)
			ctx := b.Context()

			b.ResetTimer()

			for i := range b.N {
				var wg sync.WaitGroup
				wg.Add(numGoroutines)

				for j := range numGoroutines {
					// Each goroutine must write to a different log ID.
					// If they all wrote to the same ID, they would just create
					// artificial version conflicts, and we'd be benchmarking
					// contention and retry logic, not throughput.
					// This simulates many different aggregates being modified at once.
					logID := event.LogID(fmt.Sprintf("concurrent-log-%d-%d", i, j))

					go func(id event.LogID) {
						defer wg.Done()
						_, err := log.AppendEvents(ctx, id, version.CheckExact(0), rawEvents)
						assert.NoError(b, err)
					}(logID)
				}

				wg.Wait()
			}
		})
	}
}
