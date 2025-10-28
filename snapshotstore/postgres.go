package snapshotstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/pkg/migrations"
)

// Compile-time check to ensure Postgres store implements the SnapshotStore interface.
var _ aggregate.SnapshotStore[aggregate.ID, aggregate.Snapshot[aggregate.ID]] = (*Postgres[aggregate.ID, aggregate.Snapshot[aggregate.ID]])(
	nil,
)

// Postgres provides a PostgreSQL-backed implementation of the aggregate.SnapshotStore interface.
// It stores snapshots in the `chronicle_snapshots` table, using an "UPSERT" operation
// to always keep the latest snapshot for each aggregate. Schema management is handled
// via `goose` migrations.
type Postgres[TID aggregate.ID, TS aggregate.Snapshot[TID]] struct {
	db             *sql.DB
	encoder        encoding.Codec
	createSnapshot func() TS
	mopts          migrations.Options
}

// PostgresOption is a function that configures a Postgres store instance.
type PostgresOption[TID aggregate.ID, TS aggregate.Snapshot[TID]] func(*Postgres[TID, TS])

// WithPGMigrations allows customizing the migration behavior, such as skipping migrations
// or providing a custom logger.
func WithPGMigrations[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	options migrations.Options,
) PostgresOption[TID, TS] {
	return func(p *Postgres[TID, TS]) {
		p.mopts = options
	}
}

//go:embed postgresmigrations/*.sql
var postgresMigrations embed.FS

// NewPostgres creates and returns a new PostgreSQL-backed snapshot store.
//
// It requires a database connection, a factory function for creating new snapshot instances,
// and an encoder for serializing/deserializing snapshot data.
//
// Upon initialization, it runs database migrations using goose to ensure the necessary
// `chronicle_snapshots` table exists. The snapshot data is stored in a `BYTEA` column,
// so a binary encoder (like Protobuf or Gob) is appropriate.
func NewPostgres[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	db *sql.DB,
	createSnapshot func() TS,
	opts ...PostgresOption[TID, TS],
) (*Postgres[TID, TS], error) {
	s := &Postgres[TID, TS]{
		db:             db,
		createSnapshot: createSnapshot,
		encoder:        encoding.NewJSONB(),
		mopts: migrations.Options{
			SkipMigrations: false,
			Logger:         migrations.NopLogger(),
		},
	}

	for _, opt := range opts {
		opt(s)
	}

	if s.mopts.SkipMigrations {
		return s, nil
	}

	if err := migrations.RunMigrations(migrations.Migrations{
		DB:      db,
		Fsys:    postgresMigrations,
		Logger:  s.mopts.Logger,
		Dialect: "pgx",
		Dir:     "postgresmigrations",
	}); err != nil {
		return nil, fmt.Errorf("new postgres snapshot store: %w", err)
	}

	return s, nil
}

// SaveSnapshot encodes the provided snapshot and saves it to the PostgreSQL database.
// It uses an UPSERT (INSERT ... ON CONFLICT) operation to either create a new snapshot record
// or update the existing one for the aggregate.
func (s *Postgres[TID, TS]) SaveSnapshot(ctx context.Context, snapshot TS) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := snapshot.ID().String()

	data, err := s.encoder.Encode(snapshot)
	if err != nil {
		return fmt.Errorf("save snapshot: encode: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
        INSERT INTO chronicle_snapshots (log_id, version, data)
        VALUES ($1, $2, $3)
        ON CONFLICT (log_id) DO UPDATE SET
            version = EXCLUDED.version,
            data = EXCLUDED.data;
    `, id, snapshot.Version(), data)
	if err != nil {
		return fmt.Errorf("save snapshot: exec upsert: %w", err)
	}

	return nil
}

// GetSnapshot retrieves the latest snapshot for a given aggregate ID from the database.
// It returns the decoded snapshot, a boolean indicating if a snapshot was found,
// and an error if one occurred. If no snapshot is found for the ID, it returns `false`
// and a nil error, which is the expected behavior.
func (s *Postgres[TID, TS]) GetSnapshot(
	ctx context.Context,
	aggregateID TID,
) (TS, bool, error) {
	var empty TS
	if err := ctx.Err(); err != nil {
		return empty, false, err
	}

	id := aggregateID.String()
	var data []byte

	const qGetSnapshot = "SELECT data FROM chronicle_snapshots WHERE log_id = $1"
	err := s.db.QueryRowContext(ctx, qGetSnapshot, id).Scan(&data)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// This is not a fatal error, but the expected outcome when no snapshot exists.
			return empty, false, nil
		}
		return empty, false, fmt.Errorf("get snapshot: query row: %w", err)
	}

	// A snapshot was found, now decode it.
	snapshot := s.createSnapshot()
	if err := s.encoder.Decode(data, snapshot); err != nil {
		return empty, false, fmt.Errorf("get snapshot: decode: %w", err)
	}

	return snapshot, true, nil
}
