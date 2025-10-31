package event

import "context"

// SyncProjection processes events synchronously within the same transaction
// that appends them. It receives ALL events from a single append operation
// as a batch, ensuring atomic updates to both the event log and projection.
//
// Use cases:
//   - Transactional outbox pattern
//   - Denormalized read models requiring strong consistency
//   - Cross-aggregate invariant enforcement
type SyncProjection[TX any] interface {
	MatchesEvent(eventName string) bool
	// Handle processes a batch of events within a transaction.
	// All events are from the same AppendEvents call.
	Handle(ctx context.Context, tx TX, records []*Record) error
}
