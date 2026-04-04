# `event.TransactionalEventLog` and `aggregate.TransactionalRepository`

These are the two interfaces that help us create projections easier.

Starting with `event.TransactionalEventLog`
```go
package event

type TransactionalEventLog[TX any] interface {
	TransactionalLog[TX]
	Transactor[TX]
}

type Transactor[TX any] interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx TX) error) error
}

type TransactionalLog[TX any] interface {
	AppendInTx(
		ctx context.Context,
		tx TX,
		id LogID,
		expected version.Check,
		events RawEvents,
	) (version.Version, []*Record, error)
	Reader
}
```

This is an interface that defines an `Append` method that also provides the `TX` (transactional) type. It's implemented by the following event logs: postgres, sqlite and memory.

An `aggregate.TransactionalRepository` uses this kind of event log to orchestrate processors.

A processor is called inside an active transaction and provides us access to the root aggregate and to the committed events.

```go
type TransactionalAggregateProcessor[TX any, TID ID, E event.Any, R Root[TID, E]] interface {
	// Process is called by the TransactionalRepository *inside* an active transaction,
	// immediately after the aggregate's events have been successfully saved to the event log.
	// It receives the transaction handle, the aggregate in its new state, and the
	// strongly-typed events that were just committed.
	//
	// Returns an error if processing fails. This will cause the entire transaction to be
	// rolled back, including the saving of the events. Returns nil on success.
	Process(ctx context.Context, tx TX, root R, events CommittedEvents[E]) error
}
```
