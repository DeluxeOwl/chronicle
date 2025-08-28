package accountv2

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

type AccountsWithNameProcessor struct{}

func NewAccountsWithNameProcessor(db *sql.DB) (*AccountsWithNameProcessor, error) {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS projection_accounts (
            account_id TEXT PRIMARY KEY,
            holder_name TEXT NOT NULL
        );
    `)
	if err != nil {
		return nil, fmt.Errorf("new accounts with name processor: %w", err)
	}

	return &AccountsWithNameProcessor{}, nil
}

func (p *AccountsWithNameProcessor) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *Account,
	events aggregate.CommittedEvents[AccountEvent],
) error {
	for evt := range events.All() {
		// We only care about accountOpened events.
		if opened, ok := evt.(*accountOpened); ok {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO projection_accounts (account_id, holder_name) 
                VALUES (?, ?)
                ON CONFLICT(account_id) DO UPDATE SET 
                    holder_name = excluded.holder_name
            `, root.ID(), opened.HolderName)
			if err != nil {
				return fmt.Errorf("insert account: %w", err)
			}
		}
	}

	return nil
}
