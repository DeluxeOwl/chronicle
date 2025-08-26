package event

import (
	"context"
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/version"
)

// Reader is responsible for retrieving a sequence of events for a specific aggregate,
// identified by its LogID. It forms the read-side of an event log.
//
// Usage:
//
//	// store is an implementation of event.Log
//	records := store.ReadEvents(ctx, "account-123", version.SelectFromBeginning)
//	for record, err := range records {
//	    if err != nil {
//	        // handle error
//	    }
//	    // process record
//	}
//
// Returns a Records iterator, which allows for lazy, stream-like processing of the
// event history for a single aggregate.
type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) Records
}

// Appender is responsible for writing new events to an aggregate's log atomically.
// It uses an optimistic concurrency check via the `expected` version parameter to
// prevent race conditions.
//
// Usage:
//
//	// store is an implementation of event.Log
//	// rawEvents is a slice of event.Raw
//	// expectedVersion is the version of the aggregate before new events are applied
//	newVersion, err := store.AppendEvents(ctx, "account-123", version.CheckExact(expectedVersion), rawEvents)
//
// Returns the new version of the aggregate after the events have been successfully appended.
// If the optimistic concurrency check fails, it returns a `version.ConflictError`.
type Appender interface {
	AppendEvents(
		ctx context.Context,
		id LogID,
		expected version.Check,
		events RawEvents,
	) (version.Version, error)
}

// Log combines the Reader and Appender interfaces to represent a standard event log
// that can read from and write to an aggregate's event stream.
type Log interface {
	Reader
	Appender
}

// Records is an iterator for a sequence of *Record instances for a single aggregate.
// It allows for lazy processing, which is efficient for aggregates with long event histories
// as it avoids loading all events into memory at once.
//
// Usage:
//
//	records := store.ReadEvents(ctx, "account-123", version.SelectFromBeginning)
//	for record, err := range records {
//	    if err != nil {
//	        // handle error
//	    }
//	    fmt.Printf("Event: %s, Version: %d\n", record.EventName(), record.Version())
//	}
type Records iter.Seq2[*Record, error]

// Collect consumes the entire Records iterator and loads all event records into a slice in memory.
// This is useful when you need to have all events available before processing.
//
// Usage:
//
//	recordsIterator := store.ReadEvents(ctx, "account-123", version.SelectFromBeginning)
//	allRecords, err := recordsIterator.Collect()
//	if err != nil {
//	    // handle error
//	}
//
// Returns a slice of *Record and the first error encountered during iteration.
func (r Records) Collect() ([]*Record, error) {
	collected := []*Record{}
	for record, err := range r {
		if err != nil {
			return nil, fmt.Errorf("records collect: %w", err)
		}
		collected = append(collected, record)
	}
	return collected, nil
}

// GlobalLog extends a standard Log with the ability to read all events across
// all aggregates, in the global order they were committed. This is useful for
// building projections and other system-wide read models.
type GlobalLog interface {
	Reader
	Appender
	GlobalReader
}

// GlobalReader defines the contract for reading the global, chronologically-ordered stream
// of all events from the event store.
//
// This is implemented by event logs that can maintain a global, ordered version of ALL events,
// not just events scoped to a single log ID. The global version should always start at 1 (not 0)
// for compatibility with SQL databases.
//
// Usage:
//
//	// store is an implementation of event.GlobalReader
//	// Read all events starting from global version 1
//	allEvents := store.ReadAllEvents(ctx, version.Selector{ From: 1 })
//	for gRecord, err := range allEvents {
//	    // process each event from the global log
//	}
type GlobalReader interface {
	ReadAllEvents(ctx context.Context, globalSelector version.Selector) GlobalRecords
}

// GlobalRecords is an iterator for the global sequence of *GlobalRecord instances.
// It allows for lazy processing of all events in the store, ordered by their global version.
//
// Usage:
//
//	// globalLog is an implementation of event.GlobalLog
//	globalRecords := globalLog.ReadAllEvents(ctx, version.Selector{From: 123})
//	for gRec, err := range globalRecords {
//	    if err != nil {
//	        // handle error
//	    }
//	    // process gRec
//	}
type GlobalRecords iter.Seq2[*GlobalRecord, error]

// Collect consumes the entire GlobalRecords iterator and loads all global event records
// into a slice in memory.
//
// Usage:
//
//	// globalLog is an implementation of event.GlobalLog
//	globalIterator := globalLog.ReadAllEvents(ctx, version.Selector{From: 1})
//	allGlobalRecords, err := globalIterator.Collect()
//	if err != nil {
//	    // handle error
//	}
//
// Returns a slice of *GlobalRecord and the first error encountered during iteration.
func (r GlobalRecords) Collect() ([]*GlobalRecord, error) {
	collected := []*GlobalRecord{}
	for record, err := range r {
		if err != nil {
			return nil, fmt.Errorf("records collect: %w", err)
		}
		collected = append(collected, record)
	}
	return collected, nil
}
