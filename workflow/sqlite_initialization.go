package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mattn/go-sqlite3"
)

// Concurrent first opens can fail while switching journal mode, before any
// schema statement runs. Retry only SQLite lock errors and only during startup.
// Repeating additive schema statements is safe after a partial initialization.
func retrySQLiteInitialization(ctx context.Context, initialize func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		err := initialize(ctx)
		if err == nil {
			return nil
		}
		var sqliteErr sqlite3.Error
		if !errors.As(err, &sqliteErr) ||
			(sqliteErr.Code != sqlite3.ErrBusy && sqliteErr.Code != sqlite3.ErrLocked) {
			return err
		}
		if waitErr := waitForWorker(ctx, 25*time.Millisecond); waitErr != nil {
			return fmt.Errorf("initialize SQLite workflow schema: %w", errors.Join(err, waitErr))
		}
	}
}
