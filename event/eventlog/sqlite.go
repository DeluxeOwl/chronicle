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

var (
	_ event.Log                       = new(Sqlite)
	_ event.Transactor[*sql.Tx]       = new(Sqlite)
	_ event.TransactionalLog[*sql.Tx] = new(Sqlite)
)

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

func SqliteTableName(tableName string) SqliteOption {
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
	SqliteTableName("chronicle_events")(sqliteLog)

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

	stmt, err := tx.PrepareContext(ctx, s.qInsertEvent)
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
			parts := strings.SplitN(err.Error(), conflictErrorPrefix, 2)

			if len(parts) == 2 {
				actualVersion, parseErr := strconv.ParseUint(parts[1], 10, 64)
				if parseErr == nil {
					return version.Zero, nil, version.NewConflictError(
						version.Version(exp),
						version.Version(actualVersion),
					)
				}
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
