package aggregate

import (
	"fmt"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
)

type ID interface {
	fmt.Stringer
}

type Aggregate[E event.Any] interface {
	Apply(E) error
}

type UncommitedEventsFlusher interface {
	FlushUncommitedEvents() []event.Event
}

type (
	Root[TypeID ID, TEvent event.Any] interface {
		Aggregate[TEvent]
		UncommitedEventsFlusher
		event.EventLister

		ID() TypeID
		Version() version.Version

		// Base implements these, so you *have* to embed Base.
		setVersion(version.Version)
		recordThat(Aggregate[event.Any], ...event.Event) error
	}
)

type anyRoot[TypeID ID, TEvent event.Any] struct {
	internalRoot Root[TypeID, TEvent]
}

func (a *anyRoot[TypeID, TEvent]) Apply(evt event.Any) error {
	anyEvt, ok := evt.(TEvent)
	if !ok {
		return &DataIntegrityError[TEvent]{Event: evt}
	}

	return a.internalRoot.Apply(anyEvt)
}

func RecordEvent[TypeID ID, TEvent event.Any](root Root[TypeID, TEvent], e event.Any) error {
	r := &anyRoot[TypeID, TEvent]{
		internalRoot: root,
	}

	return root.recordThat(r, event.New(e))
}

func RecordEvents[TypeID ID, TEvent event.Any](root Root[TypeID, TEvent], events ...event.Any) error {
	r := &anyRoot[TypeID, TEvent]{
		internalRoot: root,
	}

	evs := make([]event.Event, len(events))
	for i := range events {
		evs[i] = event.New(events[i])
	}

	return root.recordThat(r, evs...)
}

func LoadFromRecords[TypeID ID, TEvent event.Any](root Root[TypeID, TEvent], registry event.Registry, events event.Records) error {
	for e, err := range events {
		if err != nil {
			return fmt.Errorf("load from records: %w", err)
		}

		fact, ok := registry.NewEventFactory(e.EventName())
		if !ok {
			return fmt.Errorf("load from records: factory not registered for event %q", e.EventName())
		}

		evt := fact()
		if err := encoding.Unmarshal(e.Data(), evt); err != nil {
			return fmt.Errorf("load from records: unmarshal record data: %w", err)
		}

		anyEvt, ok := evt.(TEvent)
		if !ok {
			return fmt.Errorf("load from records: %w", &DataIntegrityError[TEvent]{Event: evt})
		}

		if err := root.Apply(anyEvt); err != nil {
			return fmt.Errorf("load from records: root apply: %w", err)
		}

		root.setVersion(e.Version())
	}

	return nil
}
