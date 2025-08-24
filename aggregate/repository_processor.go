package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
)

// TransactionalAggregateProcessor defines a contract for processing an aggregate
// and its committed events within the same transaction as the save operation.
// This is a high-level, type-safe hook that is useful for atomically updating
// read models (projections) or creating outbox messages.
//
// The 'Process' method is called by a TransactionalRepository *inside* an active transaction,
// immediately after the aggregate's events have been successfully saved to the event log.
// This guarantees that the event log write and any side effects performed by the processor
// (like updating a projection table or inserting an outbox message) either all succeed or all fail together.
//
// TX is the transaction handle type (e.g., *sql.Tx).
// TID is the aggregate's ID type.
// E is the aggregate's base event type.
// R is the aggregate root type.
//
// Usage (Outbox Pattern with *sql.Tx):
//
//	// Assume an outbox table:
//	// CREATE TABLE outbox_messages (id SERIAL PRIMARY KEY, event_name TEXT, payload JSONB);
//
//	import (
//		"database/sql"
//		"github.com/DeluxeOwl/chronicle/examples/internal/account"
//	)
//
//	type OutboxProcessor struct {
//		// ... dependencies like a logger
//	}
//
//	func (p *OutboxProcessor) Process(
//		ctx context.Context,
//		tx *sql.Tx, // The active database transaction
//		root *account.Account,
//		events CommittedEvents[account.AccountEvent],
//	) error {
//		stmt, err := tx.PrepareContext(ctx, "INSERT INTO outbox_messages (event_name, payload) VALUES ($1, $2)")
//		if err != nil {
//			return fmt.Errorf("prepare outbox insert: %w", err)
//		}
//		defer stmt.Close()
//
//		for evt := range events.All() {
//			payload, err := json.Marshal(evt)
//			if err != nil {
//				return fmt.Errorf("marshal event %s for outbox: %w", evt.EventName(), err)
//			}
//
//			if _, err := stmt.ExecContext(ctx, evt.EventName(), payload); err != nil {
//				return fmt.Errorf("insert event %s into outbox: %w", evt.EventName(), err)
//			}
//		}
//		return nil
//	}
//
//	// Then, wire it into a transactional repository:
//	// outboxProcessor := &OutboxProcessor{}
//	// repo, err := NewTransactionalRepository(
//	//     postgresEventLog, // a transactional event log
//	//     account.NewEmpty,
//	//     nil, // transformers
//	//     outboxProcessor,
//	// )
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
