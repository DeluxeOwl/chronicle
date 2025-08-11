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
		flushUncommittedEvents() flushedUncommittedEvents
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
	if evt == nil {
		return errors.New("nil event")
	}

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
	transformers []event.Transformer[E],
	id TID,
	selector version.Selector,
) error {
	logID := event.LogID(id.String())
	recordedEvents := store.ReadEvents(ctx, logID, selector)

	if err := LoadFromRecords(ctx, root, registry, deserializer, transformers, recordedEvents); err != nil {
		return fmt.Errorf("read and load from store: %w", err)
	}

	if root.Version() == 0 {
		return errors.New("read and load from store: root not found")
	}

	return nil
}

func LoadFromRecords[TID ID, E event.Any](
	ctx context.Context,
	root Root[TID, E],
	registry event.Registry[E],
	deserializer serde.BinaryDeserializer,
	transformers []event.Transformer[E],
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

		transformedEvt := evt
		// Note: transformers need to happen in reverse order
		// e.g. encrypt -> compress
		// inverse is decompress -> decrypt
		for i := len(transformers) - 1; i >= 0; i-- {
			transformedEvt, err = transformers[i].TransformForRead(ctx, transformedEvt)
			if err != nil {
				return fmt.Errorf(
					"load from records: read transform for event %q (version %d) failed: %w",
					record.EventName(),
					record.Version(),
					err,
				)
			}
		}

		if err := root.Apply(transformedEvt); err != nil {
			return fmt.Errorf("load from records: root apply: %w", err)
		}

		root.setVersion(record.Version())
	}

	return nil
}

type (
	UncommittedEvents[E event.Any] []E
)

func FlushUncommittedEvents[TID ID, E event.Any, R Root[TID, E]](
	root R,
) UncommittedEvents[E] {
	flushedUncommitted := root.flushUncommittedEvents()
	uncommitted := make([]E, len(flushedUncommitted))

	for i, evt := range flushedUncommitted {
		concrete, ok := event.AnyToConcrete[E](evt)
		if !ok {
			assert.Never("any to concrete")
		}

		uncommitted[i] = concrete
	}

	return uncommitted
}

func RawEventsFromUncommitted[E event.Any](
	ctx context.Context,
	serializer serde.BinarySerializer,
	transformers []event.Transformer[E],
	uncommitted UncommittedEvents[E],
) ([]event.Raw, error) {
	rawEvents := make([]event.Raw, len(uncommitted))
	for i, evt := range uncommitted {
		transformedEvt := evt
		var err error

		for _, t := range transformers {
			transformedEvt, err = t.TransformForWrite(ctx, transformedEvt)
			if err != nil {
				return nil, fmt.Errorf(
					"raw events from uncommitted: write transform failed for event %q: %w",
					evt.EventName(),
					err,
				)
			}
		}

		bytes, err := serializer.SerializeBinary(transformedEvt)
		if err != nil {
			return nil, fmt.Errorf("raw events from uncommitted: %w", err)
		}

		rawEvents[i] = event.NewRaw(transformedEvt.EventName(), bytes)
	}

	return rawEvents, nil
}

type CommittedEvents[E event.Any] []E

func (committed CommittedEvents[E]) All() iter.Seq[E] {
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
	transformers []event.Transformer[E],
	root R,
) (version.Version, CommittedEvents[E], error) {
	uncommittedEvents := FlushUncommittedEvents(root)

	if len(uncommittedEvents) == 0 {
		return version.Zero, nil, nil // Nothing to save
	}

	logID := event.LogID(root.ID().String())

	// This logic correctly calculates the version before the new events were applied
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(uncommittedEvents)),
	)

	rawEvents, err := RawEventsFromUncommitted(ctx, serializer, transformers, uncommittedEvents)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("aggregate commit: events to raw: %w", err)
	}

	newVersion, err := store.AppendEvents(ctx, logID, expectedVersion, rawEvents)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("aggregate commit: append events: %w", err)
	}

	// These events now become committed
	return newVersion, CommittedEvents[E](uncommittedEvents), nil
}

func CommitEventsWithTX[TX any, TID ID, E event.Any, R Root[TID, E]](
	ctx context.Context,
	transactor event.Transactor[TX],
	txLog event.TransactionalLog[TX],
	processor TransactionalAggregateProcessor[TX, TID, E, R],
	serializer serde.BinarySerializer,
	transformers []event.Transformer[E],
	root R,
) (version.Version, CommittedEvents[E], error) {
	var newVersion version.Version
	var committedEvents CommittedEvents[E]

	uncommittedEvents := FlushUncommittedEvents(root)

	if len(uncommittedEvents) == 0 {
		return root.Version(), nil, nil // Nothing to save
	}

	logID := event.LogID(root.ID().String())

	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(uncommittedEvents)),
	)

	rawEvents, err := RawEventsFromUncommitted(ctx, serializer, transformers, uncommittedEvents)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("aggregate commit with tx: events to raw: %w", err)
	}

	err = transactor.WithinTx(ctx, func(ctx context.Context, tx TX) error {
		// Append events to the log *within the transaction*
		v, _, err := txLog.AppendInTx(ctx, tx, logID, expectedVersion, rawEvents)
		if err != nil {
			return fmt.Errorf("append events in tx: %w", err)
		}

		newVersion = v
		committedEvents = CommittedEvents[E](uncommittedEvents)

		// Call the high-level, type-safe aggregate processor in the same transaction
		if processor != nil {
			if err := processor.Process(ctx, tx, root, committedEvents); err != nil {
				return fmt.Errorf("aggregate processor: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return version.Zero, nil, fmt.Errorf("aggregate commit with tx: %w", err)
	}

	return newVersion, committedEvents, nil
}
