package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type SnapshotStrategy[TID ID, E event.Any, R Root[TID, E]] interface {
	ShouldSnapshot(
		// The current context of the Save operation.
		ctx context.Context,
		// The current root
		root R,
		// The version of the aggregate *before* the new events were committed.
		previousVersion version.Version,
		// The new version of the aggregate *after* the events were committed.
		newVersion version.Version,
		// The events that were just committed in this Save operation.
		committedEvents event.CommitedEvents,
	) bool
}

const DefaultSnapshotFrequency = 100 // A sensible default

type everyNEventsStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	N uint64
}

func (s *everyNEventsStrategy[TID, E, R]) ShouldSnapshot(_ context.Context, _ R, previousVersion, newVersion version.Version, _ event.CommitedEvents) bool {
	if s.N == 0 {
		return false
	}
	nextSnapshotVersion := (uint64(previousVersion)/s.N + 1) * s.N
	return uint64(newVersion) >= nextSnapshotVersion
}

func SnapshotEveryNEvents[TID ID, E event.Any, R Root[TID, E]](_ R, n uint64) *everyNEventsStrategy[TID, E, R] {
	return &everyNEventsStrategy[TID, E, R]{N: n}
}

type onEventsStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	eventsToMatch map[string]struct{}
}

func (s *onEventsStrategy[TID, E, R]) ShouldSnapshot(_ context.Context, _ R, _, _ version.Version, committedEvents event.CommitedEvents) bool {
	for _, committedEvent := range committedEvents {
		if _, ok := s.eventsToMatch[committedEvent.EventName()]; ok {
			return true
		}
	}

	return false
}

// OnEvent returns a strategy that creates a snapshot if a specific event was committed.
func OnEvent[TID ID, E event.Any, R Root[TID, E]](eventNames ...string) *onEventsStrategy[TID, E, R] {
	eventsToMatch := make(map[string]struct{}, len(eventNames))
	for _, name := range eventNames {
		eventsToMatch[name] = struct{}{}
	}

	return &onEventsStrategy[TID, E, R]{
		eventsToMatch: eventsToMatch,
	}
}
