package aggregate

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
)

type ID interface {
	fmt.Stringer
}

type IDer[TID ID] interface {
	ID() TID
}

type Aggregate[TID ID, E event.Any] interface {
	Apply(E) error
	IDer[TID]
}

type anyAggregate interface {
	Apply(event.Any) error
}

type Versioner interface {
	Version() version.Version
}

type (
	Root[TID ID, E event.Any] interface {
		Aggregate[TID, E]
		Versioner
		event.ConstructorProvider

		// Base implements these, so you *have* to embed Base.
		flushUncommitedEvents() event.UncommitedEvents
		setVersion(version.Version)
		recordThat(anyAggregate, ...event.Event) error
	}
)

type anyRoot[TID ID, E event.Any] struct {
	internalRoot Root[TID, E]
}

func (a *anyRoot[TID, E]) Apply(evt event.Any) error {
	anyEvt, ok := evt.(E)
	if !ok {
		return &DataIntegrityError[E]{Event: evt}
	}

	return a.internalRoot.Apply(anyEvt)
}

func (a *anyRoot[TID, E]) ID() TID {
	return a.internalRoot.ID()
}

func RecordEvent[TID ID, E event.Any](root Root[TID, E], e event.Any) error {
	r := &anyRoot[TID, E]{
		internalRoot: root,
	}

	return root.recordThat(r, event.New(e))
}

func RecordEvents[TID ID, E event.Any](root Root[TID, E], events ...event.Any) error {
	r := &anyRoot[TID, E]{
		internalRoot: root,
	}

	evs := make([]event.Event, len(events))
	for i := range events {
		evs[i] = event.New(events[i])
	}

	return root.recordThat(r, evs...)
}

func ReadAndLoadFromStore[TID ID, E event.Any](
	ctx context.Context,
	root Root[TID, E],
	store event.Log,
	registry event.Registry,
	serde event.Serializer,
	id TID,
	selector version.Selector,
) error {
	logID := event.LogID(id.String())
	recordedEvents := store.ReadEvents(ctx, logID, selector)

	if err := LoadFromRecords(root, registry, serde, recordedEvents); err != nil {
		return fmt.Errorf("read and load from store: %w", err)
	}

	if root.Version() == 0 {
		return errors.New("read and load from store: root not found")
	}

	return nil
}

func LoadFromRecords[TID ID, E event.Any](
	root Root[TID, E],
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

		anyEvt, ok := evt.(E)
		if !ok {
			return fmt.Errorf("load from records: %w", &DataIntegrityError[E]{Event: evt})
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
	store event.Log,
	serializer event.Serializer,
	root R,
) (version.Version, event.CommitedEvents, error) {
	uncommitedEvents := FlushUncommitedEvents(root)

	if len(uncommitedEvents) == 0 {
		return root.Version(), event.CommitedEvents{}, nil // Nothing to save
	}

	logID := event.LogID(root.ID().String())

	// This logic correctly calculates the version before the new events were applied
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(uncommitedEvents)),
	)

	rawEvents, err := uncommitedEvents.ToRaw(serializer)
	if err != nil {
		return root.Version(), event.CommitedEvents{}, fmt.Errorf("aggregate commit: events to raw: %w", err)
	}

	newVersion, err := store.AppendEvents(ctx, logID, expectedVersion, rawEvents)
	if err != nil {
		return root.Version(), event.CommitedEvents{}, fmt.Errorf("aggregate commit: append events: %w", err)
	}

	// These events now become committed
	return newVersion, event.CommitedEvents(uncommitedEvents), nil
}
