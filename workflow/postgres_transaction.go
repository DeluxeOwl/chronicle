package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
)

// DefaultPostgresTransactionTimeout bounds short workflow storage transactions.
// It does not limit the duration of activity execution.
const DefaultPostgresTransactionTimeout = 5 * time.Second

// Client cancellation alone cannot free locks when transaction responses are
// blackholed. Install server limits before the callback acquires any locks.
func runPostgresWorkflowTx(
	ctx context.Context,
	db *sql.DB,
	timeout time.Duration,
	fn func(context.Context, *sql.Tx) error,
) error {
	txCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	tx, err := db.BeginTx(txCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: false})
	if err != nil {
		return fmt.Errorf("postgres workflow begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // Also rolls back on panic; a committed transaction cannot roll back.

	// PostgreSQL accepts integer milliseconds. Round up so a positive
	// sub-millisecond duration never disables a server timeout.
	milliseconds := timeout.Milliseconds()
	if timeout%time.Millisecond != 0 {
		milliseconds++
	}
	if milliseconds < 1 {
		milliseconds = 1
	}
	limit := strconv.FormatInt(milliseconds, 10) + "ms"
	if _, err := tx.ExecContext(txCtx, `
		SELECT set_config('idle_in_transaction_session_timeout', $1, true),
			set_config('statement_timeout', $1, true)
	`, limit); err != nil {
		return fmt.Errorf("postgres workflow configure transaction timeouts: %w", err)
	}
	if err := fn(txCtx, tx); err != nil {
		return err
	}
	if err := txCtx.Err(); err != nil {
		return fmt.Errorf("postgres workflow commit transaction: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres workflow commit transaction: %w", errors.Join(err, txCtx.Err()))
	}
	return nil
}

// Bound event append before the event log acquires its stream advisory lock,
// not merely when the queue processor is invoked after the append.
type postgresWorkflowTransactor struct {
	db      *sql.DB
	timeout time.Duration
}

func (p *postgresWorkflowTransactor) WithinTx(
	ctx context.Context,
	fn func(context.Context, *sql.Tx) error,
) error {
	return runPostgresWorkflowTx(ctx, p.db, p.timeout, fn)
}

var _ event.Transactor[*sql.Tx] = (*postgresWorkflowTransactor)(nil)
