package eventlog

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var (
	_ event.GlobalLog                      = (*Postgres)(nil)
	_ event.Log                            = (*Postgres)(nil)
	_ event.TransactionalEventLog[*sql.Tx] = (*Postgres)(nil)
)

// Postgres is an implementation of event.Log for a PostgreSQL database.
// It uses a dedicated table for events and a PL/pgSQL function with a trigger
// to enforce optimistic concurrency control at the database level.
// This approach is highly reliable as it prevents race conditions during writes.
//
// See `NewPostgres` for initialization.
type Postgres struct {
	db       *sql.DB
	useByteA bool

	qCreateTable      string
	qCreateFunction   string
	qCreateTrigger    string
	qInsertEvent      string
	qReadEvents       string
	qReadAllEvents    string
	qDeleteEventsUpTo string
}

type PostgresOption func(*Postgres)

// PostgresTableName is a configuration option that sets the name of the table
// used to store events. If not provided, it defaults to "chronicle_events".
// This option also regenerates all internal SQL queries to use the specified table name.
//
// Usage:
//
//	pgLog, err := eventlog.NewPostgres(db,
//	    eventlog.PostgresTableName("my_domain_events"),
//	)
//
// Returns a `PostgresOption` to be used with `NewPostgres`.
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

		p.qCreateFunction = fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION chronicle_check_event_version()
		RETURNS TRIGGER AS $$
		DECLARE max_version BIGINT;
		BEGIN
			-- Acquire a transaction-level advisory lock based on a hash of the log_id.
			-- This serializes inserts for the same log_id, preventing the race condition
			-- where two transactions simultaneously try to insert the first event for a stream.
			-- The lock is automatically released at the end of the transaction.
			PERFORM pg_advisory_xact_lock(hashtext(NEW.log_id));

			-- Now that we have the lock, we can safely check the current version.
			SELECT version INTO max_version FROM %s WHERE log_id = NEW.log_id ORDER BY version DESC LIMIT 1;
			
			-- If no row was found (i.e., this is the first event for this log_id),
			-- the 'max_version' will be NULL. COALESCE handles this, setting it to 0.
			IF NOT FOUND THEN
				max_version := 0;
			END IF;

			IF NEW.version != max_version + 1 THEN
				-- Raise an exception with the actual latest version to be parsed by the client.
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
			"SELECT version, event_name, data FROM %s WHERE log_id = $1 AND version >= $2 AND ($3 = 0 OR version <= $4) ORDER BY version ASC",
			tableName,
		)
		p.qReadAllEvents = fmt.Sprintf(
			"SELECT global_version, version, log_id, event_name, data FROM %s WHERE global_version >= $1 AND ($2 = 0 OR global_version <= $3) ORDER BY global_version ASC",
			tableName,
		)
		p.qDeleteEventsUpTo = fmt.Sprintf(
			"DELETE FROM %s WHERE log_id = $1 AND version <= $2",
			tableName,
		)
	}
}

// PostgresUseBYTEA configures the event log to use a BYTEA column for event data
// instead of the default JSONB. This is useful if you are using a binary
// serialization format like Protobuf instead of JSON.
//
// Usage:
//
//	pgLog, err := eventlog.NewPostgres(db,
//	    eventlog.PostgresUseBYTEA(),
//	)
//
// Returns a `PostgresOption` to be used with `NewPostgres`.
func PostgresUseBYTEA() PostgresOption {
	return func(p *Postgres) {
		p.useByteA = true
	}
}

// NewPostgres creates a new Postgres event log. Upon initialization, it ensures that
// the necessary database schema (table, function, and trigger) is created. This
// setup is performed within a transaction, making it safe to call on application startup.
//
// By default, this log uses a JSONB column and expects a JSON-based
// serializer (e.g., serde.NewJSONBinary()) to be configured in the repository.
// Use the `PostgresUseBYTEA` option if you plan to use a different binary format.
//
// Usage:
//
//	db, err := sql.Open("postgres", "user=... password=... dbname=... sslmode=disable")
//	if err != nil {
//	    log.Fatal(err)
//	}
//	pgLog, err := eventlog.NewPostgres(db)
//	if err != nil {
//	    log.Fatal(err)
//	}
//
// Returns a configured `*Postgres` instance or an error if the setup fails.
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

// AppendEvents writes a batch of raw events for a given aggregate ID to the log.
// It wraps the entire operation in a new database transaction to ensure atomicity.
//
// Usage:
//
//	newVersion, err := pgLog.AppendEvents(ctx, logID, expectedVersion, rawEvents)
//	if err != nil {
//	    var conflictErr *version.ConflictError
//	    if errors.As(err, &conflictErr) {
//	        // handle optimistic concurrency failure
//	    }
//	}
//
// Returns the new version of the aggregate after the append, or an error.
// A `version.ConflictError` is returned if the expected version does not match.
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

// AppendInTx writes events within an existing database transaction.
// It relies on the `trg_chronicle_check_event_version` trigger in the database to
// perform the optimistic concurrency check. If the check fails, the trigger
// raises an exception which is parsed into a `version.ConflictError`.
//
// This method is primarily for internal use by `TransactionalRepository` or advanced scenarios.
//
// Returns the new aggregate version, the records that were created, and an error if the operation fails.
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
//	err := pgLog.WithinTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
//	    // Perform database operations with tx
//	    return nil
//	})
//
// Returns an error if the transaction fails to begin, commit, or if the
// provided function returns an error.
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

// ReadEvents retrieves the event history for a single aggregate, starting from
// a specified version. It returns an iterator for efficiently processing the stream.
//
// Usage:
//
//	records := pgLog.ReadEvents(ctx, logID, version.SelectFromBeginning)
//	for record, err := range records {
//	    // process record
//	}
//
// Returns an `event.Records` iterator.
func (p *Postgres) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		rows, err := p.db.QueryContext(
			ctx,
			p.qReadEvents,
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
//	globalRecords := pgLog.ReadAllEvents(ctx, version.Selector{From: 1})
//	for gRecord, err := range globalRecords {
//	    // process global record for a projection
//	}
//
// Returns an `event.GlobalRecords` iterator.
func (p *Postgres) ReadAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		rows, err := p.db.QueryContext(
			ctx,
			p.qReadAllEvents,
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
func (p *Postgres) DangerouslyDeleteEventsUpTo(
	ctx context.Context,
	id event.LogID,
	version version.Version,
) error {
	_, err := p.db.ExecContext(ctx, p.qDeleteEventsUpTo, id, version)
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
