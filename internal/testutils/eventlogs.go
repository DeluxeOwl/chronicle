package testutils

import (
	"database/sql"
	"os"
	"testing"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/stretchr/testify/require"
)

type EventLog struct {
	Name string
	Log  event.Log
}

type GlobalEventLog struct {
	Name string
	Log  event.GlobalLog
}

//nolint:dupl // not needed.
func SetupEventLogs(t *testing.T) ([]EventLog, func()) {
	t.Helper()

	//nolint:exhaustruct // not needed.
	pebbleDB, err := pebble.Open("", &pebble.Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)

	pebbleLog := eventlog.NewPebble(pebbleDB)
	require.NoError(t, err)

	f, err := os.CreateTemp(t.TempDir(), "sqlite-*.db")
	require.NoError(t, err)

	sqliteDB, err := sql.Open("sqlite3", f.Name())
	require.NoError(t, err)

	sqliteLog, err := eventlog.NewSqlite(sqliteDB)
	require.NoError(t, err)

	pg, cleanupPostgres := SetupPostgres(t)
	postgresLog, err := eventlog.NewPostgres(pg)
	require.NoError(t, err)

	return []EventLog{
			{
				Name: "memory log",
				Log:  eventlog.NewMemory(),
			},
			{
				Name: "pebble memory log",
				Log:  pebbleLog,
			},
			{
				Name: "sqlite log",
				Log:  sqliteLog,
			},
			{
				Name: "postgres log",
				Log:  postgresLog,
			},
		}, func() {
			err := pebbleDB.Close()
			require.NoError(t, err)

			err = sqliteDB.Close()
			require.NoError(t, err)

			cleanupPostgres()
		}
}

//nolint:dupl // not needed.
func SetupGlobalEventLogs(t *testing.T) ([]GlobalEventLog, func()) {
	t.Helper()

	//nolint:exhaustruct // not needed.
	pebbleDB, err := pebble.Open("", &pebble.Options{
		FS: vfs.NewMem(),
	})
	require.NoError(t, err)

	pebbleLog := eventlog.NewPebble(pebbleDB)
	require.NoError(t, err)

	f, err := os.CreateTemp(t.TempDir(), "sqlite-*.db")
	require.NoError(t, err)

	sqliteDB, err := sql.Open("sqlite3", f.Name())
	require.NoError(t, err)

	sqliteLog, err := eventlog.NewSqlite(sqliteDB)
	require.NoError(t, err)

	pg, cleanupPostgres := SetupPostgres(t)
	postgresLog, err := eventlog.NewPostgres(pg)
	require.NoError(t, err)

	return []GlobalEventLog{
			{
				Name: "memory log",
				Log:  eventlog.NewMemory(),
			},
			{
				Name: "pebble memory log",
				Log:  pebbleLog,
			},
			{
				Name: "sqlite log",
				Log:  sqliteLog,
			},
			{
				Name: "postgres log",
				Log:  postgresLog,
			},
		}, func() {
			err := pebbleDB.Close()
			require.NoError(t, err)

			err = sqliteDB.Close()
			require.NoError(t, err)

			cleanupPostgres()
		}
}

func CollectRecords(t *testing.T, records event.Records) []*event.Record {
	t.Helper()
	collected, err := records.Collect()
	require.NoError(t, err)

	return collected
}
