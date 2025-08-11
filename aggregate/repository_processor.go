package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
)

// TransactionalAggregateProcessor defines a contract for processing an aggregate
// and its committed events within the same transaction as the save operation.
// This is a high-level, type-safe hook that is ideal for atomically updating
// read models (projections) or creating outbox messages.
//
// TX is the transaction handle type (e.g., *sql.Tx).
// TID is the aggregate's ID type.
// E is the aggregate's base event type.
// R is the aggregate root type.
type TransactionalAggregateProcessor[TX any, TID ID, E event.Any, R Root[TID, E]] interface {
	// Process is called by the TransactionalRepository *inside* an active transaction,
	// immediately after the aggregate's events have been successfully saved to the event log.
	// It receives the transaction handle, the aggregate in its new state, and the
	// strongly-typed events that were just committed.
	Process(ctx context.Context, tx TX, root R, events CommittedEvents[E]) error
}
