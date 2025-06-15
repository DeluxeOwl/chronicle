package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
)

type ID interface {
	fmt.Stringer
}

type Aggregate[TypeID ID, E event.Any] interface {
	Apply(E) error
	ID() TypeID
}

type anyAggregate interface {
	Apply(event.Any) error
}

type (
	Root[TypeID ID, TEvent event.Any] interface {
		Aggregate[TypeID, TEvent]
		event.ConstructorProvider

		Version() version.Version

		// Base implements these, so you *have* to embed Base.
		flushUncommitedEvents() event.UncommitedEvents
		setVersion(version.Version)
		recordThat(anyAggregate, ...event.Event) error
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

func (a *anyRoot[TypeID, TEvent]) ID() TypeID {
	return a.internalRoot.ID()
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

func LoadFromRecords[TypeID ID, TEvent event.Any](
	root Root[TypeID, TEvent],
	registry event.Registry,
	serde event.Serializer,
	records event.Records,
) error {
	for record, err := range records {
		if err != nil {
			return fmt.Errorf("load from records: %w", err)
		}

		fact, ok := registry.NewEventFactory(record.EventName())
		if !ok {
			return fmt.Errorf("load from records: factory not registered for event %q", record.EventName())
		}

		evt := fact()
		if err := serde.UnmarshalEvent(record.Data(), evt); err != nil {
			return fmt.Errorf("load from records: unmarshal record data: %w", err)
		}

		anyEvt, ok := evt.(TEvent)
		if !ok {
			return fmt.Errorf("load from records: %w", &DataIntegrityError[TEvent]{Event: evt})
		}

		if err := root.Apply(anyEvt); err != nil {
			return fmt.Errorf("load from records: root apply: %w", err)
		}

		root.setVersion(record.Version())
	}

	return nil
}

func FlushUncommitedEvents[TID ID, E event.Any, R Root[TID, E]](
	root R,
) event.UncommitedEvents {
	return root.flushUncommitedEvents()
}

// CommitEvents takes a root aggregate, flushes its uncommitted events, and appends them
// to the provided event log. It's a reusable helper for implementing any
// custom Repository's Save method.
func CommitEvents[TID ID, E event.Any, R Root[TID, E]](
	ctx context.Context,
	root R,
	serializer event.Serializer,
	store event.Log,
) error {
	// Theoretically, if we wanted to allow custom implementations
	// we could make it like so: any(root).(UncommitedEventsFlusher)
	uncommitedEvents := FlushUncommitedEvents(root)

	if len(uncommitedEvents) == 0 {
		return nil // Nothing to save
	}

	logID := event.LogID(root.ID().String())

	// This logic correctly calculates the version before the new events were applied
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(uncommitedEvents)),
	)

	rawEvents, err := uncommitedEvents.ToRaw(serializer)
	if err != nil {
		return fmt.Errorf("aggregate commit: events to raw: %w", err)
	}

	if _, err := store.AppendEvents(ctx, logID, expectedVersion, rawEvents); err != nil {
		return fmt.Errorf("aggregate commit: append events: %w", err)
	}

	return nil
}
