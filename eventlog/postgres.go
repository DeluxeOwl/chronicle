package eventlog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var (
	_ event.GlobalLog                      = (*Postgres)(nil)
	_ event.Log                            = (*Postgres)(nil)
	_ event.TransactionalEventLog[*sql.Tx] = (*Postgres)(nil)
)

// Uses a PL/pgSQL function and a trigger to perform optimistic concurrency checks.
type Postgres struct {
	db       *sql.DB
	useByteA bool

	qCreateTable    string
	qCreateFunction string
	qCreateTrigger  string
	qInsertEvent    string
	qReadEvents     string
	qReadAllEvents  string
}

type PostgresOption func(*Postgres)

func PostgresTableName(tableName string) PostgresOption {
	return func(p *Postgres) {
		dataType := "JSONB"
		if p.useByteA {
			dataType = "BYTEA"
		}

		p.qCreateTable = fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			global_version BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			log_id         TEXT NOT NULL,
			version        BIGINT NOT NULL,
			event_name     TEXT NOT NULL,
			data           %s,
			UNIQUE (log_id, version)
		);`, tableName, dataType)

		// Note PL/pgSQL function RAISE EXCEPTION includes the error message in the pq.Error.Detail field.
		// The err.Error() contains this detail, which we parse.
		// We use SELECT ... FOR UPDATE to lock the rows for the given log_id, preventing race conditions
		// where another transaction could insert an event between our read (MAX(version)) and write (INSERT).
		p.qCreateFunction = fmt.Sprintf(`
        CREATE OR REPLACE FUNCTION chronicle_check_event_version()
        RETURNS TRIGGER AS $$
        DECLARE
            max_version BIGINT;
        BEGIN
            SELECT COALESCE(MAX(version), 0) INTO max_version FROM %s WHERE log_id = NEW.log_id FOR UPDATE;
            IF NEW.version != max_version + 1 THEN
                RAISE EXCEPTION '%s%%', max_version;
            END IF;
            RETURN NEW;
        END;
        $$ LANGUAGE plpgsql;`, tableName, conflictErrorPrefix)

		p.qCreateTrigger = fmt.Sprintf(`
        DROP TRIGGER IF EXISTS trg_chronicle_check_event_version ON %s;
        CREATE TRIGGER trg_chronicle_check_event_version
        BEFORE INSERT ON %s
        FOR EACH ROW EXECUTE FUNCTION chronicle_check_event_version();
        `, tableName, tableName)

		p.qInsertEvent = fmt.Sprintf(
			"INSERT INTO %s (log_id, version, event_name, data) VALUES ($1, $2, $3, $4)",
			tableName,
		)
		p.qReadEvents = fmt.Sprintf(
			"SELECT version, event_name, data FROM %s WHERE log_id = $1 AND version >= $2 ORDER BY version ASC",
			tableName,
		)
		p.qReadAllEvents = fmt.Sprintf(
			"SELECT global_version, version, log_id, event_name, data FROM %s WHERE global_version >= $1 ORDER BY global_version ASC",
			tableName,
		)
	}
}

// PostgresUseBYTEA configures the event log to use a BYTEA column for event data.
// If not used, the default column type is JSONB.
func PostgresUseBYTEA() PostgresOption {
	return func(p *Postgres) {
		p.useByteA = true
	}
}

// NewPostgres creates a new Postgres event log. It will also create the necessary
// table, function, and trigger in the database if they don't already exist.
//
// The application is responsible for providing a JSON-based
// serializer (e.g., serde.NewJSONBinary()) to the repository. The database will
// reject non-JSON data. If you want to use a BYTEA column instead of JSONB, you can use the PostgresUseBYTEA option.
func NewPostgres(db *sql.DB, opts ...PostgresOption) (*Postgres, error) {
	//nolint:exhaustruct // Fields are set below
	pgLog := &Postgres{db: db}

	for _, o := range opts {
		o(pgLog)
	}

	if pgLog.qCreateTable == "" {
		PostgresTableName("chronicle_events")(pgLog)
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("new postgres event log: begin setup transaction: %w", err)
	}

	//nolint:errcheck // The error from Commit/Rollback will be handled.
	defer tx.Rollback()

	if _, err := tx.Exec(pgLog.qCreateTable); err != nil {
		return nil, fmt.Errorf("new postgres event log: create events table: %w", err)
	}
	if _, err := tx.Exec(pgLog.qCreateFunction); err != nil {
		return nil, fmt.Errorf("new postgres event log: create version check function: %w", err)
	}
	if _, err := tx.Exec(pgLog.qCreateTrigger); err != nil {
		return nil, fmt.Errorf("new postgres event log: create version check trigger: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("new postgres event log: commit setup transaction: %w", err)
	}

	return pgLog, nil
}

func (p *Postgres) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	var newVersion version.Version

	err := p.WithinTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		v, _, err := p.AppendInTx(ctx, tx, id, expected, events)
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

func (p *Postgres) AppendInTx(
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

	stmt, err := tx.PrepareContext(ctx, p.qInsertEvent)
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

// WithinTx executes a function within a database transaction.
func (p *Postgres) WithinTx(
	ctx context.Context,
	fn func(ctx context.Context, tx *sql.Tx) error,
) error {
	tx, err := p.db.BeginTx(ctx, nil)
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

//nolint:dupl // I think it's better to keep them separate.
func (p *Postgres) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		rows, err := p.db.QueryContext(ctx, p.qReadEvents, id, selector.From)
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

func (p *Postgres) ReadAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		rows, err := p.db.QueryContext(ctx, p.qReadAllEvents, globalSelector.From)
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
