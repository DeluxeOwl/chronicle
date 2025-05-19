package event

import (
	"context"
	"iter"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type LogID string

type RecordedEvent struct {
	Version version.Version
	LogID   LogID
	GenericEvent
}
type EventReader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) iter.Seq[RecordedEvent]
}

type EventAppender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events ...GenericEvent) (version.Version, error)
}

type Store interface {
	EventReader
	EventAppender
}

// If you want to decorate only one of the reader/appender.
type StoreComposition struct {
	EventReader
	EventAppender
}
