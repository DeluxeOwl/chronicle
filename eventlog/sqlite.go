package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var (
	_ event.GlobalLog                      = new(Sqlite)
	_ event.Log                            = new(Sqlite)
	_ event.TransactionalEventLog[*sql.Tx] = new(Sqlite)
)

var (
	ErrUnsupportedCheck = errors.New("unsupported version check type")
	ErrNoEvents         = errors.New("empty events")
)

type Sqlite struct {
	db *sql.DB
}

type SqliteOption func(*Sqlite)

func NewSqlite(db *sql.DB, opts ...SqliteOption) (*Sqlite, error) {
	sqliteLog := &Sqlite{
		db: db,
	}

	for _, o := range opts {
		o(sqliteLog)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS chronicle_events (
			global_version INTEGER PRIMARY KEY AUTOINCREMENT,
			log_id          TEXT    NOT NULL,
			version        INTEGER NOT NULL,
			event_name     TEXT    NOT NULL,
			data           BLOB,
			UNIQUE (log_id, version)
		);`); err != nil {
		return nil, fmt.Errorf("new sqlite event log: create events table failed: %w", err)
	}
	if _, err := db.Exec(`
        CREATE TRIGGER IF NOT EXISTS check_event_version
        BEFORE INSERT ON chronicle_events
        FOR EACH ROW
        BEGIN
            -- This custom message is key for our driver-agnostic error check. Must be "_chronicle_version_conflict: "
            SELECT RAISE(ABORT, '_chronicle_version_conflict: ' || (SELECT COALESCE(MAX(version), 0) FROM chronicle_events WHERE log_id = NEW.log_id))
            WHERE NEW.version != (
                SELECT COALESCE(MAX(version), 0) + 1
                FROM chronicle_events
                WHERE log_id = NEW.log_id
            );
        END;`); err != nil {
		return nil, fmt.Errorf("new sqlite event log: create version check trigger failed: %w", err)
	}

	return sqliteLog, nil
}

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

			var eventVersion uint64 // Scan into uint64, which database/sql handles from INTEGER
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
