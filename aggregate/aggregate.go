package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
)

type AggregateError string

const (
	ErrFailedToRecord AggregateError = "failed_to_record_event"
)

type ID interface {
	fmt.Stringer
}

type Aggregate interface {
	Apply(event.EventAny) error
}

type RecordedEventsFlusher interface {
	FlushRecordedEvents() []event.Event
}

type Root[TypeID ID] interface {
	Aggregate
	RecordedEventsFlusher

	ID() TypeID
	Version() version.Version

	// EventRecorder implements these, so you *have* to embed EventRecorder.
	setVersion(version.Version)
	recordThat(Aggregate, ...event.Event) error
}

func RecordEvent[TypeID ID](root Root[TypeID], e event.EventAny) error {
	return root.recordThat(root, event.New(e))
}
