package migrations

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// Logger defines the interface for a logger that can be used by the goose migration tool.
type Logger interface {
	Fatalf(format string, v ...any)
	Printf(format string, v ...any)
}

func NopLogger() Logger {
	return goose.NopLogger()
}

// Options provides configuration for the database migration process.
type Options struct {
	// SkipMigrations prevents the store from automatically running database migrations on startup.
	// This is useful in environments where migrations are handled by a separate process.
	SkipMigrations bool
	// Logger is the logger instance goose will use. It defaults to a no-op logger.
	Logger Logger
}

type Migrations struct {
	DB      *sql.DB
	Fsys    fs.FS
	Logger  Logger
	Dialect string
	Dir     string
}

const (
	migrationTimeout       = 5 * time.Minute
	migrationUnlockTimeout = 5 * time.Second
)

// This gate serializes callers in this process, including in-memory SQLite databases.
var migrationGate = make(chan struct{}, 1)

// RunMigrations runs SQL migrations with a five-minute timeout.
// See RunMigrationsContext for concurrency guarantees.
func RunMigrations(migrations Migrations) error {
	return RunMigrationsContext(context.Background(), migrations)
}

// RunMigrationsContext runs SQL migrations without changing Goose's global settings.
// The context covers database operations and waiting for other callers in this process.
// A five-minute timeout applies when the context has no earlier deadline.
//
// PostgreSQL callers also coordinate across processes with Goose's advisory lock.
// File-backed SQLite callers use a persistent .chronicle-migrate.lock sidecar next to
// the canonical database path. The directory must be writable and support file locks.
// SQLite guarantees require the same local filesystem path, not hard-link aliases
// or separate filesystem namespaces. In-memory SQLite uses only the process gate.
// The database remains open, and migration history stays in goose_db_version.
func RunMigrationsContext(ctx context.Context, migrations Migrations) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()

	select {
	case migrationGate <- struct{}{}:
		defer func() { <-migrationGate }()
	case <-ctx.Done():
		return fmt.Errorf("run migrations: wait: %w", ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("run migrations: wait: %w", err)
	}

	dialect, err := migrationDialect(migrations.Dialect)
	if err != nil {
		return fmt.Errorf("run migrations: set dialect: %w", err)
	}
	fsys, err := migrationFilesystem(migrations)
	if err != nil {
		return fmt.Errorf("run migrations: filesystem: %w", err)
	}
	logger := migrations.Logger
	if logger == nil {
		logger = NopLogger()
	}
	opts := []goose.ProviderOption{
		goose.WithLogger(logger),
		goose.WithDisableGlobalRegistry(true),
	}
	providerDialect := dialect
	if dialect == goose.DialectPostgres {
		store, err := newPostgresStore()
		if err != nil {
			return fmt.Errorf("run migrations: postgres store: %w", err)
		}
		locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 300))
		if err != nil {
			return fmt.Errorf("run migrations: session locker: %w", err)
		}
		opts = append(opts,
			goose.WithStore(store),
			goose.WithSessionLocker(boundedSessionLocker{SessionLocker: locker}),
		)
		providerDialect = goose.DialectCustom
	}
	provider, err := goose.NewProvider(providerDialect, migrations.DB, fsys, opts...)
	if err != nil {
		return fmt.Errorf("run migrations: provider: %w", err)
	}

	if dialect == goose.DialectSQLite3 {
		file, err := lockSQLiteMigrations(ctx, migrations.DB)
		if err != nil {
			return fmt.Errorf("run migrations: sqlite lock: %w", err)
		}
		if file != nil {
			defer func() { retErr = errors.Join(retErr, file.Close()) }()
		}
	}

	// Goose v3.26.0 Up calls lock-free HasPending before acquiring its session lock.
	// GetDBVersion initializes the version table under that lock before Up can query it.
	if _, err := provider.GetDBVersion(ctx); err != nil {
		return fmt.Errorf("run migrations: initialize: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("run migrations: up: %w", err)
	}
	// Provider verbosity also logs raw SQL. Keep startup logs limited to results.
	for _, result := range results {
		logger.Printf("goose: %s", result.String())
	}
	if len(results) == 0 {
		logger.Printf("goose: no migrations to run")
	} else {
		logger.Printf("goose: successfully migrated database to version: %d", results[len(results)-1].Source.Version)
	}
	return nil
}

func migrationFilesystem(migrations Migrations) (fs.FS, error) {
	if migrations.Fsys == nil {
		return os.DirFS(migrations.Dir), nil
	}
	return fs.Sub(migrations.Fsys, filepath.ToSlash(filepath.Clean(migrations.Dir)))
}

func migrationDialect(name string) (goose.Dialect, error) {
	switch name {
	case "postgres", "pgx":
		return goose.DialectPostgres, nil
	case "mysql":
		return goose.DialectMySQL, nil
	case "sqlite3", "sqlite":
		return goose.DialectSQLite3, nil
	case "mssql", "azuresql", "sqlserver":
		return goose.DialectMSSQL, nil
	case "redshift":
		return goose.DialectRedshift, nil
	case "tidb":
		return goose.DialectTiDB, nil
	case "clickhouse":
		return goose.DialectClickHouse, nil
	case "vertica":
		return goose.DialectVertica, nil
	case "ydb":
		return goose.DialectYdB, nil
	case "turso":
		return goose.DialectTurso, nil
	case "starrocks":
		return goose.DialectStarrocks, nil
	default:
		return "", fmt.Errorf("%q: unknown dialect", name)
	}
}

type boundedSessionLocker struct {
	lock.SessionLocker
}

func (l boundedSessionLocker) SessionLock(ctx context.Context, conn *sql.Conn) error {
	if err := l.SessionLocker.SessionLock(ctx, conn); err != nil {
		// A canceled acquisition can still have acquired a server-side session lock.
		// Discard the session rather than return a possibly locked connection to the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	return nil
}

func (l boundedSessionLocker) SessionUnlock(ctx context.Context, conn *sql.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, migrationUnlockTimeout)
	defer cancel()
	if err := l.SessionLocker.SessionUnlock(ctx, conn); err != nil {
		// sql.Conn.Close alone returns the connection to the pool, retaining session locks.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return err
	}
	return nil
}
