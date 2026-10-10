package queue_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/queue"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

func TestQueue_WorkSurvivesDatabaseReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	clock := &testClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	db, log := openSQLite(t, path)
	q := openQueue[string](t, log, "emails", clock.Now)
	finished := queue.Message[string]{ID: "finished", Payload: "already sent"}
	require.NoError(t, q.Enqueue(t.Context(), finished))
	require.NoError(
		t,
		q.Enqueue(t.Context(), queue.Message[string]{ID: "in-flight", Payload: "send again"}),
	)
	require.NoError(t, q.Enqueue(t.Context(), queue.Message[string]{
		ID: "scheduled", Payload: "send later", AvailableAt: clock.Now().Add(2 * time.Minute),
	}))

	done, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, done)
	require.Equal(t, "finished", done.Message.ID)
	require.NoError(t, q.Ack(t.Context(), done.Receipt))
	inFlight, err := q.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, inFlight)
	require.Equal(t, "in-flight", inFlight.Message.ID)
	require.NoError(t, db.Close())

	db, log = openSQLite(t, path)
	reopened := openQueue[string](t, log, "emails", clock.Now)
	require.NoError(t, reopened.Enqueue(t.Context(), finished))
	delivery, err := reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery, "the active lease and delayed delivery must survive restart")

	clock.Advance(time.Minute)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "in-flight", delivery.Message.ID)
	require.Equal(t, "send again", delivery.Message.Payload)
	require.Equal(t, uint64(2), delivery.Attempt)
	require.ErrorIs(t, reopened.Ack(t.Context(), inFlight.Receipt), queue.ErrLeaseLost)
	require.NoError(t, reopened.Ack(t.Context(), delivery.Receipt))

	clock.Advance(time.Minute)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.NotNil(t, delivery)
	require.Equal(t, "scheduled", delivery.Message.ID)
	require.NoError(t, reopened.Ack(t.Context(), delivery.Receipt))
	require.NoError(t, db.Close())

	_, log = openSQLite(t, path)
	reopened = openQueue[string](t, log, "emails", clock.Now)
	delivery, err = reopened.Claim(t.Context(), time.Minute)
	require.NoError(t, err)
	require.Nil(t, delivery)
}

func openSQLite(t *testing.T, path string) (*sql.DB, event.Log) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	log, err := eventlog.NewSqlite(db)
	require.NoError(t, err)
	return db, log
}
