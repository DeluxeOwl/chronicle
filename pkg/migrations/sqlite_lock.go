package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The sidecar stays on disk. Unlinking it can let another process lock a new inode
// while a waiting process still holds the old one. Closing the file releases the lock.
func lockSQLiteMigrations(ctx context.Context, db *sql.DB) (*os.File, error) {
	filename, err := sqliteMainFilename(ctx, db)
	if err != nil {
		return nil, err
	}
	if filename == "" {
		// In-memory databases cannot be shared between independent processes.
		return nil, nil //nolint:nilnil // No file lock is needed for an in-memory database.
	}
	filename, err = filepath.EvalSymlinks(filename)
	if err != nil {
		return nil, fmt.Errorf("resolve sqlite database path: %w", err)
	}
	filename, err = filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("absolute sqlite database path: %w", err)
	}
	file, err := os.OpenFile(filename+".chronicle-migrate.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open sqlite migration lock: %w", err)
	}
	if err := waitSQLiteMigrationLock(ctx, file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func sqliteMainFilename(ctx context.Context, db *sql.DB) (string, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA database_list")
	if err != nil {
		return "", fmt.Errorf("sqlite database list: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sequence int
		var name, filename string
		if err := rows.Scan(&sequence, &name, &filename); err != nil {
			return "", fmt.Errorf("scan sqlite database list: %w", err)
		}
		if name == "main" {
			return filename, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read sqlite database list: %w", err)
	}
	return "", errors.New("sqlite database list has no main database")
}

func waitSQLiteMigrationLock(ctx context.Context, file *os.File) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for sqlite migration lock: %w", err)
		}
		locked, err := trySQLiteMigrationLock(file)
		if err != nil {
			return fmt.Errorf("acquire sqlite migration lock: %w", err)
		}
		if locked {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for sqlite migration lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
