package snapshotstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/encoding"
)

// Compile-time check to ensure PostgresStore implements the SnapshotStore interface.
var _ aggregate.SnapshotStore[aggregate.ID, aggregate.Snapshot[aggregate.ID]] = (*PostgresStore[aggregate.ID, aggregate.Snapshot[aggregate.ID]])(
	nil,
)

// PostgresStore provides a PostgreSQL-backed implementation of the aggregate.SnapshotStore interface.
// It stores snapshots in a dedicated table, using an "UPSERT" operation to always keep the
// latest snapshot for each aggregate, which is identified by its log_id.
type PostgresStore[TID aggregate.ID, TS aggregate.Snapshot[TID]] struct {
	db             *sql.DB
	encoder        encoding.Codec
	createSnapshot func() TS
	useByteA       bool

	qCreateTable  string
	qSaveSnapshot string
	qGetSnapshot  string
}

// PostgresStoreOption is a function that configures a PostgresStore instance.
type PostgresStoreOption[TID aggregate.ID, TS aggregate.Snapshot[TID]] func(*PostgresStore[TID, TS])

// PostgresSnapshotTableName allows customizing the name of the table used to store snapshots.
// If not provided, it defaults to "chronicle_snapshots". This option regenerates internal SQL
// queries to use the specified table name.
//
// Usage:
//
//	store, err := snapshotstore.NewPostgresStore(db, createFunc,
//	    snapshotstore.PostgresSnapshotTableName("my_custom_snapshots"),
//	)
func PostgresSnapshotTableName[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	tableName string,
) PostgresStoreOption[TID, TS] {
	return func(p *PostgresStore[TID, TS]) {
		dataType := "JSONB"
		if p.useByteA {
			dataType = "BYTEA"
		}

		p.qCreateTable = fmt.Sprintf(`
        CREATE TABLE IF NOT EXISTS %s (
            log_id  TEXT PRIMARY KEY,
            version BIGINT NOT NULL,
            data    %s NOT NULL
        );`, tableName, dataType)

		// This "UPSERT" statement is atomic and efficient. It inserts a new snapshot
		// or, if a snapshot for the log_id already exists, updates it.
		p.qSaveSnapshot = fmt.Sprintf(`
        INSERT INTO %s (log_id, version, data)
        VALUES ($1, $2, $3)
        ON CONFLICT (log_id) DO UPDATE SET
            version = EXCLUDED.version,
            data = EXCLUDED.data;
        `, tableName)

		p.qGetSnapshot = fmt.Sprintf(
			"SELECT data FROM %s WHERE log_id = $1",
			tableName,
		)
	}
}

// PostgresSnapshotUseBYTEA configures the snapshot store to use a BYTEA column for snapshot data
// instead of the default JSONB. This is useful for binary serialization formats like Protobuf.
func PostgresSnapshotUseBYTEA[TID aggregate.ID, TS aggregate.Snapshot[TID]]() PostgresStoreOption[TID, TS] {
	return func(p *PostgresStore[TID, TS]) {
		p.useByteA = true
	}
}

// NewPostgresStore creates and returns a new PostgreSQL-backed snapshot store.
// It requires a database connection and a factory function for creating new snapshot instances,
// which is used during deserialization.
//
// Upon initialization, it ensures the necessary database table exists by executing a
// `CREATE TABLE IF NOT EXISTS` command.
func NewPostgresStore[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	db *sql.DB,
	createSnapshot func() TS,
	opts ...PostgresStoreOption[TID, TS],
) (*PostgresStore[TID, TS], error) {
	//nolint:exhaustruct // Fields are set below
	s := &PostgresStore[TID, TS]{
		db:             db,
		createSnapshot: createSnapshot,
		encoder:        encoding.NewJSONB(), // Default to JSON serialization
	}

	for _, opt := range opts {
		opt(s)
	}

	if s.qCreateTable == "" {
		PostgresSnapshotTableName[TID, TS]("chronicle_snapshots")(s)
	}

	if _, err := s.db.Exec(s.qCreateTable); err != nil {
		return nil, fmt.Errorf("new postgres snapshot store: create snapshots table: %w", err)
	}

	return s, nil
}

// SaveSnapshot serializes the provided snapshot and saves it to the PostgreSQL database.
// It uses an UPSERT (INSERT ... ON CONFLICT) operation to either create a new snapshot record
// or update the existing one for the aggregate.
func (s *PostgresStore[TID, TS]) SaveSnapshot(ctx context.Context, snapshot TS) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := snapshot.ID().String()

	data, err := s.encoder.Encode(snapshot)
	if err != nil {
		return fmt.Errorf("save snapshot: serialize: %w", err)
	}

	_, err = s.db.ExecContext(ctx, s.qSaveSnapshot, id, snapshot.Version(), data)
	if err != nil {
		return fmt.Errorf("save snapshot: exec upsert: %w", err)
	}

	return nil
}

// GetSnapshot retrieves the latest snapshot for a given aggregate ID from the database.
// It returns the deserialized snapshot, a boolean indicating if a snapshot was found,
// and an error if one occurred. If no snapshot is found for the ID, it returns `false`
// and a nil error, which is the expected behavior.
func (s *PostgresStore[TID, TS]) GetSnapshot(
	ctx context.Context,
	aggregateID TID,
) (TS, bool, error) {
	var empty TS
	if err := ctx.Err(); err != nil {
		return empty, false, err
	}

	id := aggregateID.String()
	var data []byte

	err := s.db.QueryRowContext(ctx, s.qGetSnapshot, id).Scan(&data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// This is not a fatal error. It correctly indicates that no snapshot exists for this aggregate.
			return empty, false, nil
		}
		return empty, false, fmt.Errorf("get snapshot: query row: %w", err)
	}

	// A snapshot was found, now deserialize it.
	snapshot := s.createSnapshot()
	if err := s.encoder.Decode(data, snapshot); err != nil {
		return empty, false, fmt.Errorf("get snapshot: deserialize: %w", err)
	}

	return snapshot, true, nil
}
