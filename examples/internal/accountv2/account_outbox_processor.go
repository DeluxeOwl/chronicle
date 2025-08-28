package accountv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

type AccountOutboxProcessor struct{}

func NewAccountOutboxProcessor(db *sql.DB) (*AccountOutboxProcessor, error) {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS outbox_account_events (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            aggregate_id TEXT NOT NULL,
            event_name TEXT NOT NULL,
            payload BLOB NOT NULL
        );
    `)
	if err != nil {
		return nil, fmt.Errorf("new account outbox processor: could not create table: %w", err)
	}
	return &AccountOutboxProcessor{}, nil
}

// Process writes committed AccountEvents to the outbox table within the same transaction.
func (p *AccountOutboxProcessor) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *Account,
	committedEvents aggregate.CommittedEvents[AccountEvent],
) error {
	for _, event := range committedEvents {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("outbox process: failed to marshal event: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
            INSERT INTO outbox_account_events (aggregate_id, event_name, payload) 
            VALUES (?, ?, ?)
        `, root.ID(), event.EventName(), payload)
		if err != nil {
			return fmt.Errorf("outbox process: insert event: %w", err)
		}
	}
	return nil
}
