package eventlog

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/pressly/goose/v3"
)

var (
	_ event.GlobalLog                      = (*Sqlite)(nil)
	_ event.Log                            = (*Sqlite)(nil)
	_ event.TransactionalEventLog[*sql.Tx] = (*Sqlite)(nil)
)

var (
	ErrUnsupportedCheck = errors.New("unsupported version check type")
	ErrNoEvents         = errors.New("empty events")
)

// Sqlite is an implementation of event.Log for a SQLite database.
// It uses a dedicated table for events and a trigger to enforce optimistic
// concurrency control at the database level. This approach is highly reliable
// as it prevents race conditions during writes.
//
// See `NewSqlite` for initialization.
type Sqlite struct {
	db    *sql.DB
	mopts MigrationsOptions
}

type SqliteOption func(*Sqlite)

// WithSqliteMigrations configures migration behavior for the SQLite event log.
// Use this to skip automatic migrations or provide a custom logger.
func WithSqliteMigrations(options MigrationsOptions) SqliteOption {
	return func(s *Sqlite) {
		s.mopts = options
	}
}

//go:embed sqlitemigrations/*.sql
var sqliteMigrations embed.FS

// NewSqlite creates a new Sqlite event log. Upon initialization, it ensures that
// the necessary database schema (table and trigger) is created. This setup is
// performed within a transaction, making it safe to call on application startup.
//
// IMPORTANT: By default, this log uses a BLOB column and expects a binary-based
// encoder (e.g., codec.NewGOB() or codec.NewJSONB()) to be configured in the repository.
// Modify the migrations or create your own implementation of a store if you want a different format.
//
// Usage:
//
//	db, err := sql.Open("sqlite3", "file:chronicle.db?cache=shared&mode=rwc")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	sqliteLog, err := eventlog.NewSqlite(db)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
// Returns a configured `*Sqlite` instance or an error if the setup fails.
func NewSqlite(db *sql.DB, opts ...SqliteOption) (*Sqlite, error) {
	sqliteLog := &Sqlite{
		db: db,
		mopts: MigrationsOptions{
			SkipMigrations: false,
			Logger:         goose.NopLogger(),
		},
	}

	for _, o := range opts {
		o(sqliteLog)
	}

	if sqliteLog.mopts.SkipMigrations {
		return sqliteLog, nil
	}

	goose.SetBaseFS(sqliteMigrations)
	goose.SetLogger(sqliteLog.mopts.Logger)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return nil, fmt.Errorf("new sqlite event log: %w", err)
	}

	if err := goose.Up(db, "sqlitemigrations"); err != nil {
		return nil, fmt.Errorf("new sqlite event log: %w", err)
	}

	return sqliteLog, nil
}

// AppendEvents writes a batch of raw events for a given aggregate ID to the log.
// It wraps the entire operation in a new database transaction to ensure atomicity.
//
// Usage:
//
//	newVersion, err := sqliteLog.AppendEvents(ctx, logID, expectedVersion, rawEvents)
//	if err != nil {
//	    var conflictErr *version.ConflictError
//	    if errors.As(err, &conflictErr) {
//	        // handle optimistic concurrency failure
//	    }
//	}
//
// Returns the new version of the aggregate after the append, or an error.
// A `version.ConflictError` is returned if the expected version does not match.
func (s *Sqlite) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	var newVersion version.Version

	err := s.WithinTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		v, _, err := s.AppendInTx(ctx, tx, id, expected, events)
		if err != nil {
			return err
		}
		newVersion = v
		return nil
	})
	if err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}
	return newVersion, nil
}

// AppendInTx writes events within an existing database transaction.
// It relies on the database trigger to perform the optimistic concurrency check.
// If the check fails, the trigger raises an exception which is parsed into a
// `version.ConflictError`.
//
// This method is primarily for internal use by `TransactionalRepository` or advanced scenarios.
//
// Returns the new aggregate version, the records that were created, and an error if the operation fails.
func (s *Sqlite) AppendInTx(
	ctx context.Context,
	tx *sql.Tx,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, []*event.Record, error) {
	if err := ctx.Err(); err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", err)
	}

	if len(events) == 0 {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrNoEvents)
	}

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrUnsupportedCheck)
	}

	stmt, err := tx.PrepareContext(
		ctx,
		"INSERT INTO chronicle_events (log_id, version, event_name, data) VALUES (?, ?, ?, ?)",
	)
	if err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: prepare statement: %w", err)
	}
	defer stmt.Close()

	records := events.ToRecords(id, version.Version(exp))

	for _, record := range records {
		_, err := stmt.ExecContext(
			ctx,
			record.LogID(),
			record.Version(),
			record.EventName(),
			record.Data(),
		)
		if err != nil {
			if actualVersion, isConflict := parseConflictError(err); isConflict {
				return version.Zero, nil, version.NewConflictError(
					version.Version(exp),
					actualVersion,
				)
			}

			return version.Zero, nil, fmt.Errorf("append in tx: exec statement: %w", err)
		}
	}

	newStreamVersion := version.Version(exp) + version.Version(len(events))
	return newStreamVersion, records, nil
}

// WithinTx executes a function within a database transaction. It begins a new
// transaction, executes the provided function, and then commits it. If the
// function returns an error or a panic occurs, the transaction is rolled back.
//
// Usage:
//
//	err := sqliteLog.WithinTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
//	    // Perform database operations with tx
//	    return nil
//	})
//
// Returns an error if the transaction fails to begin, commit, or if the
// provided function returns an error.
func (s *Sqlite) WithinTx(
	ctx context.Context,
	fn func(ctx context.Context, tx *sql.Tx) error,
) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("within tx: begin transaction: %w", err)
	}

	//nolint:errcheck // not needed.
	defer tx.Rollback()

	if err := fn(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("within tx: commit transaction: %w", err)
	}

	return nil
}

// ReadEvents retrieves the event history for a single aggregate, starting from
// a specified version. It returns an iterator for efficiently processing the stream.
//
// Usage:
//
//	records := sqliteLog.ReadEvents(ctx, logID, version.SelectFromBeginning)
//	for record, err := range records {
//	    // process record
//	}
//
// Returns an `event.Records` iterator.
func (s *Sqlite) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		rows, err := s.db.QueryContext(
			ctx,
			"SELECT version, event_name, data FROM chronicle_events WHERE log_id = ? AND version >= ? AND (? = 0 OR version <= ?) ORDER BY version ASC",
			id,
			selector.From,
			selector.To,
			selector.To,
		)
		if err != nil {
			yield(nil, fmt.Errorf("read events: query context: %w", err))
			return
		}
		defer rows.Close()

		for rows.Next() {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			var eventVersion uint64
			var eventName string
			var data []byte

			if err := rows.Scan(&eventVersion, &eventName, &data); err != nil {
				yield(nil, fmt.Errorf("read events: scan row: %w", err))
				return
			}

			record := event.NewRecord(version.Version(eventVersion), id, eventName, data)
			if !yield(record, nil) {
				return
			}
		}

		if err := rows.Err(); err != nil {
			yield(nil, fmt.Errorf("read events: rows error: %w", err))
		}
	}
}

// ReadAllEvents retrieves the global stream of all events across all aggregates,
// ordered chronologically by their global sequence number. This is useful for
// building projections or other system-wide consumers.
//
// Usage:
//
//	globalRecords := sqliteLog.ReadAllEvents(ctx, version.Selector{From: 1})
//	for gRecord, err := range globalRecords {
//	    // process global record for a projection
//	}
//
// Returns an `event.GlobalRecords` iterator.
func (s *Sqlite) ReadAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		rows, err := s.db.QueryContext(
			ctx,
			"SELECT global_version, version, log_id, event_name, data FROM chronicle_events WHERE global_version >= ? AND (? = 0 OR global_version <= ?) ORDER BY global_version ASC",
			globalSelector.From,
			globalSelector.To,
			globalSelector.To,
		)
		if err != nil {
			yield(nil, fmt.Errorf("read all events: query context: %w", err))
			return
		}
		defer rows.Close()

		for rows.Next() {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			var globalVersion, streamVersion uint64
			var logID, eventName string
			var data []byte

			if err := rows.Scan(&globalVersion, &streamVersion, &logID, &eventName, &data); err != nil {
				yield(nil, fmt.Errorf("read all events: scan row: %w", err))
				return
			}

			record := event.NewGlobalRecord(
				version.Version(globalVersion),
				version.Version(streamVersion),
				event.LogID(logID),
				eventName,
				data,
			)
			if !yield(record, nil) {
				return
			}
		}

		if err := rows.Err(); err != nil {
			yield(nil, fmt.Errorf("read all events: rows error: %w", err))
		}
	}
}

// ⚠️⚠️⚠️ WARNING: Read carefully
//
// DangerouslyDeleteEventsUpTo permanently deletes all events for a specific
// log ID up to and INCLUDING the specified version.
//
// This operation is irreversible and breaks the immutability of the event log.
//
// It is intended for use cases manually pruning
// event streams, and should be used with extreme caution.
//
// Rebuilding aggregates or projections after this operation may lead to an inconsistent state.
//
// It is recommended to only use this after generating a snapshot event of your aggregate state before running this.
// Remember to also invalidate projections that depend on deleted events and any snapshots older than the version you're calling this function with.
func (s *Sqlite) DangerouslyDeleteEventsUpTo(
	ctx context.Context,
	id event.LogID,
	version version.Version,
) error {
	_, err := s.db.ExecContext(
		ctx,
		"DELETE FROM chronicle_events WHERE log_id = ? AND version <= ?",
		id,
		version,
	)
	if err != nil {
		return fmt.Errorf(
			"dangerously delete events for log '%s' up to version %d: %w",
			id,
			version,
			err,
		)
	}
	return nil
}
