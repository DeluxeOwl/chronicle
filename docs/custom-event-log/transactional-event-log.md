# Transactional Event Log

The framework supports atomically saving events and updating read models or an outbox table within the same transaction. To enable this for your custom log, you need to implement two additional interfaces: `event.Transactor` and `event.TransactionalLog`.

`Transactor`'s job is to manage the transaction lifecycle. For a standard SQL database, this is straightforward.

```go
// Example skeleton for a SQL-based Transactor
func (m *MySQLEventLog) WithinTx(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err // Failed to start transaction
	}
	defer tx.Rollback() // Rollback is a no-op if the transaction is committed

	if err := fn(ctx, tx); err != nil {
		return err // The function failed, so we'll rollback
	}

	return tx.Commit() // Success
}
```

The `TransactionalLog` interface provides a version of `AppendEvents` that works with an existing transaction handle instead of creating a new one.

```go
type TransactionalLog[TX any] interface {
	AppendInTx(
		ctx context.Context,
		tx TX, // The transaction handle
		id LogID,
		expected version.Check,
		events RawEvents,
	) (version.Version, []*event.Record, error)
	Reader // A transactional log must also be a reader
}
```

The logic inside `AppendInTx` is identical to the `AppendEvents` flow described in [the `event.Appender` interface](event-appender.md), except it uses the provided transaction handle (`tx TX`) instead of creating a new one. You can look at `eventlog/postgres.go` or `eventlog/sqlite.go` for concrete examples.
