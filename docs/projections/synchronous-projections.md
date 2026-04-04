# Synchronous Projections (`event.SyncProjection`)

A `SyncProjection` is a primitive for building strongly-consistent read models. It operates synchronously, ensuring that the projection is updated within the same database transaction as the events being saved.

This approach is powerful for use cases that cannot tolerate eventual consistency, such as maintaining unique constraints across aggregates or building a transactional outbox. Unlike the aggregate-specific processors that work with type-safe events, `SyncProjection` operates on generic `event.Record`s. This gives it the flexibility to listen to events from different aggregate types, or even all events in the system.

```go
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
```

-   `MatchesEvent(eventName string) bool`: A filter method called for each newly committed event. Your projection should return `true` for event names it wants to process.
-   `Handle(ctx context.Context, tx TX, records []*Record) error`: The core processing logic. It receives the active transaction handle `tx` and a slice of `*Record`s that matched the filter. You can use the transaction to atomically update your read models. If this method returns an error, the entire transaction (including the event append) is rolled back.

This interface is orchestrated by `event.TransactableLog`, which wraps a transactional event log and a `SyncProjection` to manage the atomic updates automatically.

Use `event.NewLogWithProjection` or `event.NewTransactableLogWithProjection`. Works with most SQL and KV stores.

## Example: System Wide Constraints - Unique Usernames

You can find this example in [examples/8_unique_constraint/main.go](../examples/8_unique_constraint/main.go).

In event sourcing systems, all state changes are stored as immutable events. However, some business rules require unique constraints (e.g., unique usernames, email addresses, or account numbers) that must be enforced before events are persisted. And eventual consistency models can't guarantee uniqueness at write time.

This example demonstrates how to implement unique constraints using synchronous projections that execute within the same transaction as event persistence. 

This code uses a synchronous projection that runs in the same database transaction as event insertion: 
```go
type uniqueUsernameProjection struct{}

func (u *uniqueUsernameProjection) Handle(
    ctx context.Context,
    tx *sql.Tx,
    records []*event.Record,
) error {
    // Insert into unique constraint table within same transaction
    _, err := stmt.ExecContext(ctx, holderName.HolderName)
    if err != nil {
        // Constraint violation rolls back entire transaction
        return fmt.Errorf("username '%s' already exists: %w", holderName.HolderName, err)
    }
    return nil
}
```

It uses a dedicated constraint table - basically we moved the constraint check into the db:
```sql
CREATE TABLE unique_usernames (
    username TEXT NOT NULL UNIQUE
);
```

Running the example:
```bash
❯ go run examples/8_unique_constraint/main.go

State of unique usernames table:
┌──────────┐
│ USERNAME │
├──────────┤
│ Alice    │
└──────────┘

Attempting to create a duplicate user 'Alice'
Successfully prevented duplicate user. Error: repo save: aggregate commit with tx: append events in tx: projection handle records: username 'Alice' already exists: UNIQUE constraint failed: unique_usernames.username

Final state of unique usernames table:
┌──────────┐
│ USERNAME │
├──────────┤
│ Alice    │
└──────────┘

All events (note that the duplicate 'Alice' event was not saved):
┌──────────────────┬─────────┬─────────────────────────┬────────────────────────────
│      LOG ID      │ VERSION │       EVENT NAME        │              DATA                                                                                
├──────────────────┼─────────┼─────────────────────────┼────────────────────────────
│ alice-account-01 │ 1       │ account/opened          │ {..., "holderName":"Alice"} 
│ alice-account-01 │ 2       │ account/money_deposited │ {..., "amount":100} 
│ alice-account-01 │ 3       │ account/money_deposited │ {..., "amount":50} 

```
