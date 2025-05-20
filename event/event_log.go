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
	Envelope
}

type StoredEvents = iter.Seq[StoredEvent]

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) StoredEvents
}

type Appender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events ...Envelope) (version.Version, error)
}

type Log interface {
	Reader
	Appender
}

// If you want to decorate only one of the reader/appender.
type LogFuse struct {
	Reader
	Appender
}
