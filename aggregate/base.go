package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

// Base is a struct that should be embedded in your aggregate root.
// It provides the fundamental mechanisms for versioning and tracking uncommitted events.
// By embedding Base, your aggregate automatically satisfies several required interfaces
// for the event sourcing framework.
//
// Usage:
//
//	type MyAggregate struct {
//	    aggregate.Base
//	    // ... other fields
//	}
type Base struct {
	uncommittedEvents flushedUncommittedEvents
	version           version.Version
}

// Version returns the current version of the aggregate.
// The version starts at 0 and is incremented for each new event recorded.
//
// Returns the aggregate's current version number.
func (br *Base) Version() version.Version { return br.version }

// flushedUncommittedEvents is a private type alias for a slice of events
// that have been cleared from an aggregate's internal list, ready for persistence.
type flushedUncommittedEvents []event.Any

// flushUncommittedEvents clears the internal list of uncommitted events and returns them.
// This is an internal method used by the repository during the save process.
//
// Returns the slice of events that were pending.
func (br *Base) flushUncommittedEvents() flushedUncommittedEvents {
	flushed := br.uncommittedEvents
	br.uncommittedEvents = nil

	return flushed
}

// setVersion updates the aggregate's version number.
// This is used internally by the framework when loading an aggregate
// from the event log or from a snapshot.
func (br *Base) setVersion(v version.Version) {
	br.version = v
}

// anyEventApplier is an internal interface used for type erasure. It allows
// the Base struct to call the aggregate's type-safe `Apply` method without
// knowing the specific generic event type `E`.
type anyEventApplier interface {
	Apply(event.Any) error
}

// recordThat is the internal implementation of the "record and apply" pattern.
// It first applies the event to the aggregate to update its state, and upon success,
// appends the event to the list of uncommitted events and increments the version.
// This method is called by the public `RecordEvent` helper function.
//
// Returns an error if applying the event fails, preventing the event from being recorded.
func (br *Base) recordThat(aggregate anyEventApplier, events ...event.Any) error {
	for _, anyEvent := range events {
		if err := aggregate.Apply(anyEvent); err != nil {
			return fmt.Errorf("record events: root apply: %w", err)
		}

		br.uncommittedEvents = append(br.uncommittedEvents, anyEvent)
		br.version++
	}

	return nil
}
