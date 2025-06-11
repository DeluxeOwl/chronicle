package aggregate

import (
	"errors"
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"

	"github.com/DeluxeOwl/eventuallynow/version"
)

type AggregateError string

type ID interface {
	fmt.Stringer
}

type Aggregate[TEvent event.EventAny] interface {
	Apply(TEvent) error
}

type RecordedEventsFlusher interface {
	FlushUncommitedEvents() []event.Event
}

type (
	RegisterFunc                           = event.RegisterFunc
	Root[TypeID ID, TEvent event.EventAny] interface {
		Aggregate[TEvent]
		RecordedEventsFlusher
		event.Registerer

		ID() TypeID
		Version() version.Version

		// EventRecorder implements these, so you *have* to embed EventRecorder.
		setVersion(version.Version)
		setRegisteredEvents()
		hasRegisteredEvents() bool
		recordThat(Aggregate[event.EventAny], ...event.Event) error
	}
)

type anyRoot[TypeID ID, TEvent event.EventAny] struct {
	internalRoot Root[TypeID, TEvent]
}

func (a *anyRoot[TypeID, TEvent]) Apply(evt event.EventAny) error {
	anyEvt, ok := evt.(TEvent)
	if !ok {
		return errors.New("internal: this isn't supposed to happen (todo)")
	}

	return a.internalRoot.Apply(anyEvt)
}

// Uses the GlobalRegistry.
func RecordEvent[TypeID ID, TEvent event.EventAny](root Root[TypeID, TEvent], e event.EventAny) error {
	// Optimization for not registering the events for the same object
	if !root.hasRegisteredEvents() {
		event.GlobalRegistry.RegisterRoot(root)
		root.setRegisteredEvents()
	}

	r := &anyRoot[TypeID, TEvent]{
		internalRoot: root,
	}

	return root.recordThat(r, event.New(e))
}

// If you want a custom registry.
type RecordFunc[TypeID ID, TEvent event.EventAny] func(root Root[TypeID, TEvent], e event.EventAny) error

func NewRecordWithRegistry[TypeID ID, TEvent event.EventAny](registry event.Registry) RecordFunc[TypeID, TEvent] {
	return func(root Root[TypeID, TEvent], e event.EventAny) error {
		if !root.hasRegisteredEvents() {
			registry.RegisterRoot(root)
			root.setRegisteredEvents()
		}

		r := &anyRoot[TypeID, TEvent]{
			internalRoot: root,
		}

		return root.recordThat(r, event.New(e))
	}
}
