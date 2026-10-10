package migrations

import (
	"context"
	"fmt"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

// Goose's PostgreSQL existence check only searches current_schema(), while its
// unqualified migration queries resolve tables through the full search_path.
// Override only that check so an existing version table in public is reused.
type postgresStore struct {
	database.Store
}

var _ database.StoreExtender = (*postgresStore)(nil)

func newPostgresStore() (*postgresStore, error) {
	store, err := database.NewStore(database.DialectPostgres, goose.DefaultTablename)
	if err != nil {
		return nil, err
	}
	return &postgresStore{Store: store}, nil
}

func (s *postgresStore) TableExists(ctx context.Context, db database.DBTxConn) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", s.Tablename()).Scan(&exists); err != nil {
		return false, fmt.Errorf("check postgres migration table exists: %w", err)
	}
	return exists, nil
}
