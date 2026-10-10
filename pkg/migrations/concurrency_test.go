package migrations_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DeluxeOwl/chronicle/pkg/migrations"
	_ "github.com/mattn/go-sqlite3"
)

func TestRunMigrations_ConcurrentSQLiteInitialization(t *testing.T) {
	const callers = 8

	for range 4 {
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		path := filepath.Join(t.TempDir(), "startup.db")
		dbs := make([]*sql.DB, callers)
		for i := range dbs {
			dbs[i] = openSQLite(t, path)
		}
		// Establish WAL before concurrent first-open PRAGMAs can contend.
		if err := dbs[0].PingContext(ctx); err != nil {
			cancel()
			t.Fatal(err)
		}

		fsys := migrationFiles()
		start := make(chan struct{})
		results := make(chan error, callers)
		for _, db := range dbs {
			go func() {
				<-start
				results <- migrations.RunMigrationsContext(ctx, migrations.Migrations{
					DB:      db,
					Fsys:    fsys,
					Logger:  migrations.NopLogger(),
					Dialect: "sqlite3",
					Dir:     "embedded",
				})
			}()
		}
		close(start)

		var failures []error
		for range callers {
			select {
			case err := <-results:
				if err != nil {
					failures = append(failures, err)
				}
			case <-ctx.Done():
				cancel()
				t.Fatal("concurrent migrations did not finish within their bound")
			}
		}
		if len(failures) != 0 {
			cancel()
			t.Fatalf("concurrent initialization failed: %v", failures)
		}
		assertMigrated(t, ctx, dbs[0])
		cancel()
	}
}

func TestRunMigrationsContext_ReleasesSQLiteLockAfterFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "failure.db")
	db := openSQLite(t, path)
	files := migrationFiles()
	files["embedded/1_create.sql"].Data = []byte("-- +goose Up\nINVALID SQL;\n")
	if err := migrations.RunMigrationsContext(ctx, migrations.Migrations{
		DB:      db,
		Fsys:    files,
		Logger:  migrations.NopLogger(),
		Dialect: "sqlite3",
		Dir:     "embedded",
	}); err == nil {
		t.Fatal("invalid SQL migration unexpectedly succeeded")
	}
	if err := migrations.RunMigrationsContext(ctx, migrations.Migrations{
		DB:      db,
		Fsys:    migrationFiles(),
		Logger:  migrations.NopLogger(),
		Dialect: "sqlite3",
		Dir:     "embedded",
	}); err != nil {
		t.Fatalf("migration lock was not released after failure: %v", err)
	}
	assertMigrated(t, ctx, db)
}

func openSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=2000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func migrationFiles() fstest.MapFS {
	return fstest.MapFS{
		"embedded/1_create.sql": {Data: []byte(`-- +goose Up
CREATE TABLE startup (value TEXT NOT NULL);
-- +goose Down
DROP TABLE startup;
`)},
		"embedded/2_insert.sql": {Data: []byte(`-- +goose Up
INSERT INTO startup (value) VALUES ('initialized');
-- +goose Down
DELETE FROM startup;
`)},
	}
}

func assertMigrated(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var value string
	var count int
	if err := db.QueryRowContext(ctx, "SELECT value, COUNT(*) FROM startup").Scan(&value, &count); err != nil {
		t.Fatal(err)
	}
	if value != "initialized" || count != 1 {
		t.Fatalf(
			"migration ran more than once or used the wrong filesystem: value=%q count=%d",
			value,
			count,
		)
	}

	rows, err := db.QueryContext(ctx, "SELECT version_id FROM goose_db_version ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(versions, []int64{0, 1, 2}) {
		t.Fatalf("unexpected version history: %v", versions)
	}
}
