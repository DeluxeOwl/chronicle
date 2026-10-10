//go:build !windows

package workflow_test

import (
	"net/url"
	"testing"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/stretchr/testify/require"
)

func TestDistributedWorkflow_PostgresExistingHistoryUsesSearchPath(t *testing.T) {
	database, _ := startDistributedPostgres(t)
	admin := openDistributedDB(t, distributedPostgres, database)
	_, err := eventlog.NewPostgres(admin)
	require.NoError(t, err)
	_, err = admin.ExecContext(t.Context(), `
		CREATE SCHEMA app;
		CREATE ROLE chronicle_runtime LOGIN PASSWORD 'runtime-test-password';
		GRANT CONNECT ON DATABASE postgres TO chronicle_runtime;
		GRANT USAGE ON SCHEMA app, public TO chronicle_runtime;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO chronicle_runtime;
		GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO chronicle_runtime;
		ALTER ROLE chronicle_runtime SET search_path = app, public;
	`)
	require.NoError(t, err)
	connectionURL, err := url.Parse(database)
	require.NoError(t, err)
	connectionURL.User = url.UserPassword("chronicle_runtime", "runtime-test-password")
	limited := openDistributedDB(t, distributedPostgres, connectionURL.String())
	var canCreate bool
	require.NoError(
		t,
		limited.QueryRowContext(t.Context(), "SELECT has_schema_privilege(current_user, 'app', 'CREATE')").
			Scan(&canCreate),
	)
	require.False(
		t,
		canCreate,
		"the existing history must not require DDL permissions in the first schema",
	)
	_, err = eventlog.NewPostgres(limited)
	require.NoError(
		t,
		err,
		"a current public.goose_db_version is visible through app,public search_path",
	)
}
