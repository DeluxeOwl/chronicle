//go:build !windows

package workflow_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDistributedWorkflow_SQLiteConcurrentFreshStartup(t *testing.T) {
	distributedFreshStartup(t, distributedSQLite, filepath.Join(t.TempDir(), "fresh-workers.db"))
}

func TestDistributedWorkflow_PostgresConcurrentFreshStartup(t *testing.T) {
	database, _ := startDistributedPostgres(t)
	distributedFreshStartup(t, distributedPostgres, database)
}

func distributedFreshStartup(t *testing.T, backend, database string) {
	t.Helper()
	const count = 4
	children := make([]*distributedProcess, count)
	for i := range children {
		config := childWorker(backend, database, "startup", false, false)
		config.Role = distributedStarter
		children[i] = startDistributedProcess(t, config)
	}
	for _, child := range children {
		child.waitMessage(t, "ready")
	}
	for _, child := range children {
		child.release(t)
	}
	ids := make(map[string]bool, count)
	for _, child := range children {
		message := child.waitMessage(t, "started")
		require.NotEmpty(t, message.InstanceID)
		ids[string(message.InstanceID)] = true
		<-child.finished
		require.NoError(t, child.waitErr, child.log())
	}
	require.Len(
		t,
		ids,
		count,
		"every independent process must initialize storage and start its own workflow",
	)
}
