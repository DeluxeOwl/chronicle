package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestSQLiteQueue_InitializationRespectsCallerDeadlineWithBusyPool(t *testing.T) {
	db := setupSyncDB(t)
	connection, err := db.Conn(t.Context())
	require.NoError(t, err)
	defer connection.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err = workflow.NewSyncQueue(ctx, db)
	require.ErrorIs(
		t,
		err,
		context.DeadlineExceeded,
		"startup must not wait indefinitely for a database connection",
	)
}
