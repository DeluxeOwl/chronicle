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
	FlushUncommitedEvents() []event.Event
}

type (
	RegisterFunc    = event.RegisterFunc
	Root[TypeID ID] interface {
		Aggregate
		RecordedEventsFlusher
		event.Registerer

		ID() TypeID
		Version() version.Version

		// EventRecorder implements these, so you *have* to embed EventRecorder.
		setVersion(version.Version)
		setRegisteredEvents()
		hasRegisteredEvents() bool
		recordThat(Aggregate, ...event.Event) error
	}
)

func RecordEvent[TypeID ID](root Root[TypeID], e event.EventAny) error {
	// Optimization for not registering the events for the same object
	if !root.hasRegisteredEvents() {
		event.GlobalRegistry.RegisterRoot(root)
		root.setRegisteredEvents()
	}

	return root.recordThat(root, event.New(e))
}
