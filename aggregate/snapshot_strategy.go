package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type SnapshotStrategy func(
	// The current context of the Save operation.
	ctx context.Context,
	// The version of the aggregate *before* the new events were committed.
	previousVersion version.Version,
	// The new version of the aggregate *after* the events were committed.
	newVersion version.Version,
	// The events that were just committed in this Save operation.
	committedEvents event.CommitedEvents,
) bool

const DefaultSnapshotFrequency = 100 // A sensible default

func SnapshotEveryNEvents(n uint64) SnapshotStrategy {
	if n == 0 {
		return SnapshotNever()
	}

	return func(_ context.Context, previousVersion, newVersion version.Version, _ event.CommitedEvents) bool {
		nextSnapshotVersion := (uint64(previousVersion)/n + 1) * n
		return uint64(newVersion) >= nextSnapshotVersion
	}
}

func SnapshotNever() SnapshotStrategy {
	return func(_ context.Context, _, _ version.Version, _ event.CommitedEvents) bool {
		return false
	}
}

// OnEvent returns a strategy that creates a snapshot if a specific event was committed.
func OnEvent(eventNames ...string) SnapshotStrategy {
	if len(eventNames) == 0 {
		return SnapshotNever()
	}

	eventsToMatch := make(map[string]struct{}, len(eventNames))
	for _, name := range eventNames {
		eventsToMatch[name] = struct{}{}
	}

	return func(_ context.Context, _, _ version.Version, committedEvents event.CommitedEvents) bool {
		for _, committedEvent := range committedEvents {
			if _, ok := eventsToMatch[committedEvent.EventName()]; ok {
				return true
			}
		}

		return false
	}
}
