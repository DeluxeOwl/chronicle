package event

import (
	"context"
	"iter"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type LogID string

type StoredEvent struct {
	Version version.Version
	LogID   LogID
	GenericEvent
}

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) iter.Seq[StoredEvent]
}

type Appender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events ...GenericEvent) (version.Version, error)
}

type Log interface {
	Reader
	Appender
}

// If you want to decorate only one of the reader/appender.
type LogComposition struct {
	Reader
	Appender
}

func GenericEventsToStored(startingVersion version.Version, id LogID, events ...GenericEvent) []StoredEvent {
	recordedEvents := make([]StoredEvent, len(events))
	for i, e := range events {
		recordedEvents[i] = StoredEvent{
			//nolint:gosec // Won't be a problem in reality.
			Version:      startingVersion + version.Version(i+1),
			LogID:        id,
			GenericEvent: e,
		}
	}
	return recordedEvents
}
