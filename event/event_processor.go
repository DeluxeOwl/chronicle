package event

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/version"
)

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

type TransactionalEventLog[TX any] interface {
	TransactionalLog[TX]
	Transactor[TX]
}

// TransactableLog is an event.Log that orchestrates writes within a transaction
// and handles messages for a synchronous projection.
type TransactableLog[TX any] struct {
	transactor Transactor[TX]
	txLog      TransactionalLog[TX]
	projection SyncProjection[TX]
}

func NewLogWithProjection[TX any](
	log TransactionalEventLog[TX],
	projection SyncProjection[TX],
) *TransactableLog[TX] {
	return &TransactableLog[TX]{
		transactor: log,
		txLog:      log,
		projection: projection,
	}
}

func NewTransactableLogWithProjection[TX any](
	transactor Transactor[TX],
	txLog TransactionalLog[TX],
	projection SyncProjection[TX],
) *TransactableLog[TX] {
	return &TransactableLog[TX]{
		transactor: transactor,
		txLog:      txLog,
		projection: projection,
	}
}

func (l *TransactableLog[TX]) WithinTx(ctx context.Context, fn func(ctx context.Context, tx TX) error) error {
	return l.WithinTx(ctx, fn)
}

func (l *TransactableLog[TX]) AppendInTx(
	ctx context.Context,
	tx TX,
	id LogID,
	expected version.Check,
	events RawEvents,
) (version.Version, []*Record, error) {
	return l.AppendInTx(ctx, tx, id, expected, events)
}

func (l *TransactableLog[TX]) AppendEvents(
	ctx context.Context,
	id LogID,
	expected version.Check,
	events RawEvents,
) (version.Version, error) {
	var newVersion version.Version

	err := l.transactor.WithinTx(ctx, func(ctx context.Context, tx TX) error {
		// Write the events to the main event store log.
		v, records, err := l.txLog.AppendInTx(ctx, tx, id, expected, events)
		if err != nil {
			return fmt.Errorf("transactable log: %w", err)
		}
		newVersion = v

		// If a projection is configured, call it with the same transaction.
		if l.projection != nil {
			// Filter records that match the projection's event criteria
			var matchingRecords []*Record
			for _, record := range records {
				if l.projection.MatchesEvent(record.EventName()) {
					matchingRecords = append(matchingRecords, record)
				}
			}

			// Only call Handle if there are matching records
			if len(matchingRecords) > 0 {
				if err := l.projection.Handle(ctx, tx, matchingRecords); err != nil {
					return fmt.Errorf("transactable log: projection handle records: %w", err)
				}
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

func (l *TransactableLog[TX]) ReadEvents(
	ctx context.Context,
	id LogID,
	selector version.Selector,
) Records {
	return l.txLog.ReadEvents(ctx, id, selector)
}
