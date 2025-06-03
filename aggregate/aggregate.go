package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/internal/registry"
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
	RegisterEvents(r Registerer)

	// EventRecorder implements these, so you *have* to embed EventRecorder.
	setVersion(version.Version)
	setRegisteredEvents()
	hasRegisteredEvents() bool
	recordThat(Aggregate, ...event.Event) error
}

type Registerer interface {
	Register(eventName string, kind any)
}

func RecordEvent[TypeID ID](root Root[TypeID], e event.EventAny) error {
	if !root.hasRegisteredEvents() {
		root.RegisterEvents(registry.Registrar)
	}
	return root.recordThat(root, event.New(e))
}
