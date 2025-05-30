package event

import (
	"context"
	"iter"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type LogID string

type RecordedEvent struct {
	version version.Version
	logID   LogID
	EventAny
}

func NewRecorded(version version.Version, logID LogID, event EventAny) RecordedEvent {
	return RecordedEvent{
		version:  version,
		logID:    logID,
		EventAny: event,
	}
}

func (re *RecordedEvent) Version() version.Version {
	return re.version
}

func (re *RecordedEvent) LogID() LogID {
	return re.logID
}

type RecordedEvents = iter.Seq2[RecordedEvent, error]

type Reader interface {
	ReadEvents(ctx context.Context, id LogID, selector version.Selector) RecordedEvents
}

type AllReader interface {
	ReadAllEvents(ctx context.Context, selector version.Selector) RecordedEvents
}

type Appender interface {
	AppendEvents(ctx context.Context, id LogID, expected version.Check, events ...EventAny) (version.Version, error)
}

type Log interface {
	Reader
	Appender
}

type LogFuse struct {
	Reader
	Appender
}
