package migrations

import (
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"
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

func RunMigrations(migrations Migrations) error {
	goose.SetBaseFS(migrations.Fsys)
	goose.SetLogger(migrations.Logger)

	if err := goose.SetDialect(migrations.Dialect); err != nil {
		return fmt.Errorf("run migrations: set dialect: %w", err)
	}

	if err := goose.Up(migrations.DB, migrations.Dir); err != nil {
		return fmt.Errorf("run migrations: up: %w", err)
	}

	return nil
}
