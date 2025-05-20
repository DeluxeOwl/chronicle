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
	Envelope
}

type RecordedEvents = iter.Seq2[RecordedEvent, error]

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) RecordedEvents
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
