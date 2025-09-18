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

// ID is a constraint for an aggregate's identifier.
// Any type used as an ID must implement the `fmt.Stringer` interface.
type ID interface {
	fmt.Stringer
}

type IDer[TID ID] interface {
	ID() TID
}

// Aggregate defines the core behavior of an event-sourced aggregate.
// It must be able to change its state by applying an event and must provide its unique ID.
type Aggregate[TID ID, E event.Any] interface {
	Apply(E) error
	IDer[TID]
}

// Versioner is an interface for any type that can be versioned.
// This is typically implemented by embedding the `aggregate.Base` struct.
type Versioner interface {
	Version() version.Version
}

// Root represents the complete contract for an aggregate root in this framework.
// It combines the `Aggregate`, `Versioner`, and `event.EventFuncCreator` interfaces,
// along with internal methods for event handling.
// The easiest way to satisfy this interface is to embed `aggregate.Base` in your struct.
type (
	Root[TID ID, E event.Any] interface {
		Aggregate[TID, E]
		Versioner
		event.EventFuncCreator[E]

		// The following methods are provided by embedding `aggregate.Base`.
		flushUncommittedEvents() flushedUncommittedEvents
		setVersion(version.Version)
		recordThat(anyEventApplier, ...event.Any) error
	}
)

// anyApplier is a type-erased adapter that allows the generic Root.Apply(E) method
// to be called from non-generic code that works with `anyEventApplier`.
type anyApplier[TID ID, E event.Any] struct {
	internalRoot Root[TID, E]
}

// asAnyApplier wraps a generic aggregate Root in a non-generic `anyApplier`.
// This is an internal helper used by `RecordEvent`.
func asAnyApplier[TID ID, E event.Any](root Root[TID, E]) *anyApplier[TID, E] {
	return &anyApplier[TID, E]{
		internalRoot: root,
	}
}

// Apply performs a type assertion to convert the `event.Any` back to the
// concrete event type `E` before calling the aggregate's `Apply` method.
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

// RecordEvent is the primary function for recording a new event against an aggregate.
// It first applies the event to the aggregate to update its state, and upon success,
// adds the event to an internal list of uncommitted events to be persisted later.
//
// A recommended pattern is to create a private, type-safe wrapper for this function
// on your aggregate. This improves type safety and autocompletion within your
// command methods.
//
// Usage:
//
//	// 1. Define a private helper method on your aggregate that calls RecordEvent.
//	//    Notice the parameter is `AccountEvent`, your specific event sum type.
//	func (a *Account) recordThat(event AccountEvent) error {
//		return aggregate.RecordEvent(a, event)
//	}
//
//	// 2. Use this helper in your command methods. The Go compiler will now ensure
//	//    you only pass valid AccountEvent types.
//	func (a *Account) DepositMoney(amount int) error {
//		if amount <= 0 {
//			return errors.New("amount must be positive")
//		}
//		return a.recordThat(&moneyDeposited{Amount: amount})
//	}
//
// Returns an error if the aggregate's `Apply` method returns an error.
func RecordEvent[TID ID, E event.Any](root Root[TID, E], e E) error {
	return root.recordThat(asAnyApplier(root), e)
}

// RecordEvents is a convenience function to record multiple events in a single call.
// Each event is applied and recorded sequentially.
//
// Usage (inside a command method on your aggregate):
//
//	return aggregate.RecordEvents(a, &eventOne{}, &eventTwo{})
//
// Returns an error if any of the `Apply` calls fail.
func RecordEvents[TID ID, E event.Any](root Root[TID, E], events ...E) error {
	evs := make([]event.Any, len(events))
	for i := range events {
		evs[i] = events[i]
	}

	return root.recordThat(asAnyApplier(root), evs...)
}

var ErrRootNotFound = errors.New("root not found")

// ReadAndLoadFromStore is a framework helper that orchestrates loading an aggregate.
// It reads event records from the event store and uses them to hydrate a new instance
// of an aggregate root.
//
// Usage (typically inside a repository's Get method):
//
//	err := aggregate.ReadAndLoadFromStore(ctx, root, store, registry, ser, transformers, id, selector)
//
// Returns an error if reading from the store, deserializing, or applying any event fails.
// Returns an error if the aggregate is not found (i.e., has no events).
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
		return fmt.Errorf("read and load from store: %w", ErrRootNotFound)
	}

	return nil
}

// LoadFromRecords hydrates an aggregate root from an iterator of event records.
// For each record, it deserializes the data into a concrete event type, applies any
// transformations, and then applies the event to the root.
//
// Usage (used by `ReadAndLoadFromStore` and snapshot loading logic):
//
//	err := aggregate.LoadFromRecords(ctx, root, registry, ser, transformers, records)
//
// Returns an error if deserialization, transformation, or application of an event fails.
func LoadFromRecords[TID ID, E event.Any](
	ctx context.Context,
	root Root[TID, E],
	registry event.Registry[E],
	deserializer serde.BinaryDeserializer,
	transformers []event.Transformer[E],
	records event.Records,
) error {
	deserializedEvents := []E{}
	lastRecordedVersion := version.Zero

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
		deserializedEvents = append(deserializedEvents, evt)

		lastRecordedVersion = record.Version()
	}

	// Note: transformers need to happen in reverse order
	// e.g. encrypt -> compress
	// inverse is decompress -> decrypt
	var err error
	for i := len(transformers) - 1; i >= 0; i-- {
		deserializedEvents, err = transformers[i].TransformForRead(ctx, deserializedEvents)
		if err != nil {
			return fmt.Errorf(
				"load from records: read transform failed: %w",
				err,
			)
		}
	}

	hasRecords := false
	for _, transformedEvt := range deserializedEvents {
		hasRecords = true
		if err := root.Apply(transformedEvt); err != nil {
			return fmt.Errorf(
				"load from records: root apply %s: %w",
				transformedEvt.EventName(),
				err,
			)
		}
	}

	if hasRecords {
		// Even though you might have more events than what's recorded
		// because of transformers, the latest version is the recorded one.
		root.setVersion(lastRecordedVersion)
	}

	return nil
}

// UncommittedEvents represents a strongly-typed slice of events that have been
// recorded on an aggregate but not yet persisted to the event log.
type (
	UncommittedEvents[E event.Any] []E
)

// FlushUncommittedEvents clears the pending events from an aggregate and returns them
// as a strongly-typed slice. This is the first step in the `Save` process.
//
// Usage (called by `CommitEvents`):
//
//	uncommitted := aggregate.FlushUncommittedEvents(root)
//
// Returns a slice of the uncommitted events.
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

// RawEventsFromUncommitted converts a slice of strongly-typed uncommitted events
// into a slice of `event.Raw` events. It applies write-side transformers
// and serializes the event data during this process.
//
// Usage (called by `CommitEvents`):
//
//	rawEvents, err := aggregate.RawEventsFromUncommitted(ctx, serializer, transformers, uncommitted)
//
// Returns a slice of `event.Raw` ready for storage, or an error if transformation or serialization fails.
func RawEventsFromUncommitted[E event.Any](
	ctx context.Context,
	serializer serde.BinarySerializer,
	transformers []event.Transformer[E],
	uncommitted UncommittedEvents[E],
) ([]event.Raw, error) {
	transformedEvents := []E(uncommitted)
	var err error

	for _, t := range transformers {
		transformedEvents, err = t.TransformForWrite(ctx, transformedEvents)
		if err != nil {
			return nil, fmt.Errorf(
				"raw events from uncommitted: write transform failed: %w",
				err,
			)
		}
	}

	rawEvents := make([]event.Raw, len(transformedEvents))
	for i, transformedEvent := range transformedEvents {
		bytes, err := serializer.SerializeBinary(transformedEvent)
		if err != nil {
			return nil, fmt.Errorf(
				"raw events from uncommitted %s: %w",
				transformedEvent.EventName(),
				err,
			)
		}
		rawEvents[i] = event.NewRaw(transformedEvent.EventName(), bytes)
	}

	return rawEvents, nil
}

// CommittedEvents represents a strongly-typed slice of events that have been
// successfully persisted to the event log.
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

// CommitEvents orchestrates the process of persisting an aggregate's pending changes.
// It is a reusable helper for implementing a repository's `Save` method.
// It flushes events, calculates the expected version, prepares them for storage,
// and appends them to the event log.
//
// Usage (inside a repository's `Save` method):
//
//	newVersion, committed, err := aggregate.CommitEvents(ctx, store, serializer, transformers, root)
//
// Returns the new version of the aggregate, a slice of the events that were just committed,
// and an error if persistence fails (e.g., `version.ConflictError`).
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

// CommitEventsWithTX is a transactional version of `CommitEvents`.
// It saves aggregate events and invokes a `TransactionalAggregateProcessor` within the
// same database transaction, ensuring atomicity. This is ideal for updating read models
// or handling outbox patterns.
//
// Usage (inside a transactional repository's `Save` method):
//
//	newVersion, committed, err := aggregate.CommitEventsWithTX(
//	    ctx, transactor, txLog, processor, serializer, transformers, root)
//
// Returns the new aggregate version, the committed events, and an error if the transaction fails.
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
