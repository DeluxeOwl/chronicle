package event

import "context"

// Outbox defines the contract for staging messages within a transaction.
// The user implements this interface for their specific database and schema.
// T is the transaction handle type, e.g., *sql.Tx or *pebble.Batch.
type Outbox[T any] interface {
	// Stage is called by the framework *inside* an active transaction,
	// just after events have been successfully written to the event log.
	// It receives the transaction handle and the newly created event records.
	Stage(ctx context.Context, tx T, records []*Record) error
}
