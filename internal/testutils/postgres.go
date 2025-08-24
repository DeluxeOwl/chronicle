package testutils

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"

	_ "github.com/jackc/pgx/v5/stdlib" // Import the pgx stdlib driver
	_ "github.com/mattn/go-sqlite3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func SetupPostgres(t *testing.T) (*sql.DB, func()) {
	t.Helper()
	ctx := t.Context()

	user := "user"
	password := "password"
	dbName := "test-db"

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:16-alpine",
			ExposedPorts: []string{"5432"},
			WaitingFor:   wait.ForListeningPort(nat.Port("5432")),
			Env: map[string]string{
				"POSTGRES_DB":       dbName,
				"POSTGRES_USER":     user,
				"POSTGRES_PASSWORD": password,
			},
		},
		Started: true,
	})
	require.NoError(t, err)

	endpoint, err := container.Endpoint(ctx, "")
	require.NoError(t, err)

	dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		user,
		password,
		endpoint,
		dbName)

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)

	err = db.PingContext(ctx)
	require.NoError(t, err)

	cleanup := func() {
		if err := container.Terminate(ctx); err != nil {
			t.Logf("failed to terminate container: %s", err)
		}
	}

	return db, cleanup
}
