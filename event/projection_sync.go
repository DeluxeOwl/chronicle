package event

import "context"

// SyncProjection defines the contract for processing messages within a transaction.
// The user implements this interface for their specific database and schema.
// T is the transaction handle type, e.g., *sql.Tx.
// It can be used as an outbox, or to create projections.
type SyncProjection[TX any] interface {
	MatchesEvent(eventName string) bool
	// Handle is called by the framework *inside* an active transaction,
	// just after events have been successfully written to the event log.
	// It receives the transaction handle and the newly created event records.
	Handle(ctx context.Context, tx TX, records []*Record) error
}
