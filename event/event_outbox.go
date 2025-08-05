package event

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/version"
)

//go:generate go run github.com/matryer/moq@latest -pkg eventlog_test -skip-ensure -rm -out ./eventlog/outbox_mock_test.go . Outbox

// Outbox defines the contract for staging messages within a transaction.
// The user implements this interface for their specific database and schema.
// T is the transaction handle type, e.g., *sql.Tx or *pebble.Batch.
type Outbox[T any] interface {
	// Stage is called by the framework *inside* an active transaction,
	// just after events have been successfully written to the event log.
	// It receives the transaction handle and the newly created event records.
	Stage(ctx context.Context, tx T, records []*Record) error
}

type Transactor[T any] interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, tx T) error) error
}

type TransactionalLog[T any] interface {
	AppendInTx(
		ctx context.Context,
		tx T,
		id LogID,
		expected version.Check,
		events RawEvents,
	) (version.Version, []*Record, error)
	Reader
}

type TransactionalEventLog[T any] interface {
	TransactionalLog[T]
	Transactor[T]
}

// TransactableLog is an event.Log that orchestrates writes within a transaction
// and stages messages for an outbox.
type TransactableLog[T any] struct {
	transactor Transactor[T]
	txLog      TransactionalLog[T]
	outbox     Outbox[T]
}

func NewLogWithOutbox[T any](
	log TransactionalEventLog[T],
	outbox Outbox[T],
) *TransactableLog[T] {
	return &TransactableLog[T]{
		transactor: log,
		txLog:      log,
		outbox:     outbox,
	}
}

func NewTransactableLogWithOutbox[T any](
	transactor Transactor[T],
	txLog TransactionalLog[T],
	outbox Outbox[T],
) *TransactableLog[T] {
	return &TransactableLog[T]{
		transactor: transactor,
		txLog:      txLog,
		outbox:     outbox,
	}
}

func (l *TransactableLog[T]) AppendEvents(
	ctx context.Context,
	id LogID,
	expected version.Check,
	events RawEvents,
) (version.Version, error) {
	var newVersion version.Version

	err := l.transactor.WithinTx(ctx, func(ctx context.Context, tx T) error {
		// Write the events to the main event store log.
		v, records, err := l.txLog.AppendInTx(ctx, tx, id, expected, events)
		if err != nil {
			return fmt.Errorf("transactable log: %w", err)
		}
		newVersion = v

		// If an outboxer is configured, call it with the same transaction.
		if l.outbox != nil {
			if err := l.outbox.Stage(ctx, tx, records); err != nil {
				return fmt.Errorf("transactable log: stage outbox: %w", err)
			}
		}

		// If we get here with no error, the transactor will commit.
		return nil
	})
	if err != nil {
		return version.Zero, err
	}

	return newVersion, nil
}

func (l *TransactableLog[T]) ReadEvents(
	ctx context.Context,
	id LogID,
	selector version.Selector,
) Records {
	return l.txLog.ReadEvents(ctx, id, selector)
}
