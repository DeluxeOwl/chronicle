package eventlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var _ event.Log = new(Sqlite)

var (
	ErrUnsupportedCheck = errors.New("unsupported version check type")
	ErrNoEvents         = errors.New("empty events")
)

const conflictErrorPrefix = "_chronicle_version_conflict: "

type Sqlite struct {
	db *sql.DB

	// Pre-computed query strings for performance and to avoid Sprintf in hot paths.
	qCreateTable   string
	qCreateTrigger string
	qInsertEvent   string
	qReadEvents    string
}

type SqliteOption func(*Sqlite)

func WithTableName(tableName string) SqliteOption {
	return func(s *Sqlite) {
		s.qCreateTable = fmt.Sprintf(`
        CREATE TABLE IF NOT EXISTS %s (
            logID      TEXT    NOT NULL,
            version    INTEGER NOT NULL,
            event_name TEXT    NOT NULL,
            data       BLOB,
            PRIMARY KEY (logID, version)
        );`, tableName)

		s.qCreateTrigger = fmt.Sprintf(`
        CREATE TRIGGER IF NOT EXISTS check_event_version
        BEFORE INSERT ON %s
        FOR EACH ROW
        BEGIN
            -- This custom message is key for our driver-agnostic error check.
            SELECT RAISE(ABORT, '%s' || (SELECT COALESCE(MAX(version), 0) FROM %s WHERE logID = NEW.logID))
            WHERE NEW.version != (
                SELECT COALESCE(MAX(version), 0) + 1
                FROM %s
                WHERE logID = NEW.logID
            );
        END;`, tableName, conflictErrorPrefix, tableName, tableName)

		s.qInsertEvent = fmt.Sprintf(
			"INSERT INTO %s (logID, version, event_name, data) VALUES (?, ?, ?, ?)",
			tableName,
		)
		s.qReadEvents = fmt.Sprintf(
			"SELECT version, event_name, data FROM %s WHERE logID = ? AND version >= ? ORDER BY version ASC",
			tableName,
		)
	}
}

func NewSqlite(db *sql.DB, opts ...SqliteOption) (*Sqlite, error) {
	//nolint:exhaustruct // Fields are set below
	sqliteLog := &Sqlite{db: db}

	// Set default queries
	WithTableName("chronicle_events")(sqliteLog)

	for _, o := range opts {
		o(sqliteLog)
	}

	if _, err := db.Exec(sqliteLog.qCreateTable); err != nil {
		return nil, fmt.Errorf("new sqlite event log: create events table failed: %w", err)
	}
	if _, err := db.Exec(sqliteLog.qCreateTrigger); err != nil {
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
	if err := ctx.Err(); err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}

	if len(events) == 0 {
		return version.Zero, fmt.Errorf("append events: %w", ErrNoEvents)
	}

	expectedVersion, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, fmt.Errorf("append events: %w", ErrUnsupportedCheck)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return version.Zero, fmt.Errorf("append events: begin transaction: %w", err)
	}

	//nolint:errcheck // We don't care about the error here.
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, s.qInsertEvent)
	if err != nil {
		return version.Zero, fmt.Errorf("append events: prepare statement: %w", err)
	}
	defer stmt.Close()

	eventRecords := events.ToRecords(id, version.Version(expectedVersion))

	for _, record := range eventRecords {
		_, err := stmt.ExecContext(
			ctx,
			record.LogID(),
			record.Version(),
			record.EventName(),
			record.Data(),
		)
		if err != nil {
			parts := strings.SplitN(err.Error(), conflictErrorPrefix, 2)

			if len(parts) == 2 {
				actualVersion, parseErr := strconv.ParseUint(parts[1], 10, 64)
				if parseErr == nil {
					return version.Zero, version.NewConflictError(
						version.Version(expectedVersion),
						version.Version(actualVersion),
					)
				}
			}

			return version.Zero, fmt.Errorf("append events: transaction failed: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return version.Zero, fmt.Errorf("append events: commit transaction: %w", err)
	}

	newStreamVersion := version.Version(expectedVersion) + version.Version(len(events))
	return newStreamVersion, nil
}

func (s *Sqlite) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		rows, err := s.db.QueryContext(ctx, s.qReadEvents, id, selector.From)
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
