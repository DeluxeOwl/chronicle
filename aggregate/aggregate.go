package aggregate

import (
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
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
	Root[TypeID ID, TEvent event.EventAny] interface {
		Aggregate[TEvent]
		RecordedEventsFlusher
		event.EventLister

		ID() TypeID
		Version() version.Version

		// EventRecorder implements these, so you *have* to embed EventRecorder.
		setVersion(version.Version)
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

func RecordEvent[TypeID ID, TEvent event.EventAny](root Root[TypeID, TEvent], e event.EventAny) error {
	r := &anyRoot[TypeID, TEvent]{
		internalRoot: root,
	}

	return root.recordThat(r, event.New(e))
}
