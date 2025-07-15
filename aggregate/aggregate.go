package aggregate

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"

	"github.com/DeluxeOwl/chronicle/internal/assert"

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

type Versioner interface {
	Version() version.Version
}

type (
	Root[TID ID, E event.Any] interface {
		Aggregate[TID, E]
		Versioner
		event.EventFuncCreator[E]

		// Base implements these, so you *have* to embed Base.
		flushUncommitedEvents() flushedUncommitedEvents
		setVersion(version.Version)
		recordThat(anyEventApplier, ...event.Any) error
	}
)

type anyApplier[TID ID, E event.Any] struct {
	internalRoot Root[TID, E]
}

func asAnyApplier[TID ID, E event.Any](root Root[TID, E]) *anyApplier[TID, E] {
	return &anyApplier[TID, E]{
		internalRoot: root,
	}
}

func (a *anyApplier[TID, E]) Apply(evt event.Any) error {
	anyEvt, ok := evt.(E)
	if !ok {
		var empty E
		return fmt.Errorf(
			"data integrity error: loaded event of type %T but aggregate expects type %T",
			evt, empty,
		)
	}

	return a.internalRoot.Apply(anyEvt)
}

func RecordEvent[TID ID, E event.Any](root Root[TID, E], e E) error {
	return root.recordThat(asAnyApplier(root), e)
}

func RecordEvents[TID ID, E event.Any](root Root[TID, E], events ...E) error {
	evs := make([]event.Any, len(events))
	for i := range events {
		evs[i] = events[i]
	}

	return root.recordThat(asAnyApplier(root), evs...)
}

func ReadAndLoadFromStore[TID ID, E event.Any](
	ctx context.Context,
	root Root[TID, E],
	store event.Log,
	registry event.Registry[E],
	deserializer serde.BinaryDeserializer,
	id TID,
	selector version.Selector,
) error {
	logID := event.LogID(id.String())
	recordedEvents := store.ReadEvents(ctx, logID, selector)

	if err := LoadFromRecords(root, registry, deserializer, recordedEvents); err != nil {
		return fmt.Errorf("read and load from store: %w", err)
	}

	if root.Version() == 0 {
		return errors.New("read and load from store: root not found")
	}

	return nil
}

func LoadFromRecords[TID ID, E event.Any](
	root Root[TID, E],
	registry event.Registry[E],
	deserializer serde.BinaryDeserializer,
	records event.Records,
) error {
	for record, err := range records {
		if err != nil {
			return fmt.Errorf("load from records: %w", err)
		}

		fact, ok := registry.GetFunc(record.EventName())
		if !ok {
			return fmt.Errorf(
				"load from records: factory not registered for event %q",
				record.EventName(),
			)
		}

		evt := fact()
		if err := deserializer.DeserializeBinary(record.Data(), evt); err != nil {
			return fmt.Errorf("load from records: unmarshal record data: %w", err)
		}

		if err := root.Apply(evt); err != nil {
			return fmt.Errorf("load from records: root apply: %w", err)
		}

		root.setVersion(record.Version())
	}

	return nil
}

type (
	UncommitedEvents[E event.Any] []E
)

func FlushUncommitedEvents[TID ID, E event.Any, R Root[TID, E]](
	root R,
) UncommitedEvents[E] {
	flushedUncommited := root.flushUncommitedEvents()
	uncommitted := make([]E, len(flushedUncommited))

	for i, evt := range flushedUncommited {
		concrete, ok := event.AnyToConcrete[E](evt)
		if !ok {
			assert.Never("any to concrete")
		}

		uncommitted[i] = concrete
	}

	return uncommitted
}

func (uncommitted UncommitedEvents[E]) ToRaw(
	serializer serde.BinarySerializer,
) ([]event.Raw, error) {
	rawEvents := make([]event.Raw, len(uncommitted))
	for i, evt := range uncommitted {
		bytes, err := serializer.SerializeBinary(evt)
		if err != nil {
			return nil, fmt.Errorf("convert events: %w", err)
		}

		rawEvents[i] = event.NewRaw(evt.EventName(), bytes)
	}

	return rawEvents, nil
}

type CommitedEvents[E event.Any] []E

func (committed CommitedEvents[E]) All() iter.Seq[E] {
	return func(yield func(E) bool) {
		for _, evt := range committed {
			if !yield(evt) {
				return
			}
		}
	}
}

// CommitEvents takes a root aggregate, flushes its uncommitted events, and appends them
// to the provided event log. It's a reusable helper for implementing any
// custom Repository's Save method.
func CommitEvents[TID ID, E event.Any, R Root[TID, E]](
	ctx context.Context,
	store event.Log,
	serializer serde.BinarySerializer,
	root R,
) (version.Version, CommitedEvents[E], error) {
	uncommitedEvents := FlushUncommitedEvents(root)

	if len(uncommitedEvents) == 0 {
		return version.Zero, nil, nil // Nothing to save
	}

	logID := event.LogID(root.ID().String())

	// This logic correctly calculates the version before the new events were applied
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(uncommitedEvents)),
	)

	rawEvents, err := uncommitedEvents.ToRaw(serializer)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("aggregate commit: events to raw: %w", err)
	}

	newVersion, err := store.AppendEvents(ctx, logID, expectedVersion, rawEvents)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("aggregate commit: append events: %w", err)
	}

	// These events now become committed
	return newVersion, CommitedEvents[E](uncommitedEvents), nil
}
