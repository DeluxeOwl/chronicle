//go:build !windows

package workflow_test

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Both backends run the same public-API failure scenarios in independent
// processes. Postgres adds a networked store suitable for separate hosts.
func TestDistributedWorkflow_SQLite(t *testing.T) {
	runDistributedCases(
		t,
		distributedSQLite,
		func(t *testing.T) string { return filepath.Join(t.TempDir(), "workflows.db") },
	)
}

func TestDistributedWorkflow_Postgres(t *testing.T) {
	dsn := startDistributedPostgres(t)
	admin := openDistributedDB(t, distributedPostgres, dsn)
	counter := 0
	runDistributedCases(t, distributedPostgres, func(t *testing.T) string {
		counter++
		name := fmt.Sprintf("chronicle_workflow_%d", counter)
		_, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
			require.NoError(t, err)
		})
		databaseURL, err := url.Parse(dsn)
		require.NoError(t, err)
		databaseURL.Path = "/" + name
		return databaseURL.String()
	})
}

func startDistributedPostgres(t *testing.T) string {
	t.Helper()
	const port nat.Port = "5432/tcp"
	connectionURL := func(host string, port nat.Port) string {
		return "postgres://user:password@" + net.JoinHostPort(
			host,
			port.Port(),
		) + "/postgres?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:16-alpine", ExposedPorts: []string{string(port)},
			Env: map[string]string{
				"POSTGRES_USER":     "user",
				"POSTGRES_PASSWORD": "password",
				"POSTGRES_DB":       "postgres",
			},
			WaitingFor: wait.ForSQL(port, "pgx", connectionURL).
				WithStartupTimeout(40 * time.Second),
		}, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, container.Terminate(ctx))
	})
	host, err := container.Host(t.Context())
	require.NoError(t, err)
	mapped, err := container.MappedPort(t.Context(), port)
	require.NoError(t, err)
	return connectionURL(host, mapped)
}

func runDistributedCases(t *testing.T, backend string, database func(*testing.T) string) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, string, string)
	}{
		{name: "stale-checkpoint", run: distributedExpiredWorkerCannotCheckpointOverANewClaim},
		{name: "claim-expiry-after-lock", run: distributedClaimExpiryAfterLockWait},
		{name: "process-kill", run: distributedKilledWorkerResumesFromCommittedCheckpoint},
		{name: "competing-workers", run: distributedIndependentWorkersCompete},
		{name: "cross-process-publication", run: distributedPublicationSurvivesExitAndCrash},
		{name: "publication-rollback", run: distributedPublicationFailureRollsBackPayloadAndWakeup},
		{
			name: "timeout-publication-order",
			run:  distributedPostgresTimeoutCheckpointObservesPendingPublication,
		},
		{name: "heartbeat-exclusivity", run: distributedHeartbeatsKeepLongActivitiesExclusive},
		{name: "active-cancellation", run: distributedCancellationFencesAnActiveWorker},
		{
			name: "publication-registration-race",
			run:  distributedPublicationAndWaitRegistrationCannotStrandWork,
		},
		{name: "storage-partition", run: distributedStoragePartitionDoesNotLoseWork},
		{name: "blackholed-transactions", run: distributedBlackholedTransactionsReleaseLocks},
		{name: "sleep-recovery", run: func(t *testing.T, backend, database string) {
			distributedPauseSurvivesReplacement(t, backend, database, "sleep")
		}},
		{name: "retry-recovery", run: func(t *testing.T, backend, database string) {
			distributedPauseSurvivesReplacement(t, backend, database, distributedRetry)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) { test.run(t, backend, database(t)) })
	}
}

func distributedExpiredWorkerCannotCheckpointOverANewClaim(t *testing.T, backend, path string) {
	db, wf, inspector := newDistributedFixture(t, backend, path)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
	require.NoError(t, err)
	old := startDistributedProcess(t, childWorker(backend, path, "old", true, false))
	old.waitMessage(t, "ready")
	entered := old.waitMessage(t, "entered")
	require.Equal(t, id, entered.InstanceID)
	current := startDistributedProcess(t, childWorker(backend, path, "current", true, true))
	current.waitMessage(t, "ready")
	current.waitMessage(
		t,
		"entered",
	) // The old lease expired; neither claimant completed the activity.
	old.release(t)
	returned := old.waitMessage(t, "returned")
	require.True(
		t,
		returned.Stale,
		"an expired worker must not save even when the event version did not change",
	)
	old.kill()
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, detail.Summary.Status)
	require.Equal(t, 1, detail.Summary.CompletedSteps)
	current.release(t)
	result := distributedResult(t, wf, id)
	require.Equal(t, "current", result.CompletedBy)
	require.Equal(t, 1, distributedCalls(t, db, backend, id, "prepare"))
	require.Equal(
		t,
		2,
		distributedCalls(t, db, backend, id, "finish"),
		"external effects remain at least once despite checkpoint fencing",
	)
}

func distributedKilledWorkerResumesFromCommittedCheckpoint(t *testing.T, backend, path string) {
	db, wf, inspector := newDistributedFixture(t, backend, path)
	id, err := wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
	require.NoError(t, err)
	first := startDistributedProcess(t, childWorker(backend, path, "killed", true, true))
	first.waitMessage(t, "ready")
	first.waitMessage(t, "entered")
	first.kill() // SIGKILL: no defer, task acknowledgment, or graceful release can run.
	second := startDistributedProcess(t, childWorker(backend, path, "recovery", false, true))
	second.waitMessage(t, "ready")
	result := distributedResult(t, wf, id)
	require.Equal(t, "killed", result.PreparedBy)
	require.Equal(t, "recovery", result.CompletedBy)
	require.Equal(t, 1, distributedCalls(t, db, backend, id, "prepare"))
	require.Equal(t, 2, distributedCalls(t, db, backend, id, "finish"))
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	for _, event := range detail.History {
		require.NotEqual(t, "workflow/failed", event.EventName)
	}
}

func distributedIndependentWorkersCompete(t *testing.T, backend, path string) {
	db, wf, _ := newDistributedFixture(t, backend, path)
	processes := make([]*distributedProcess, 3)
	for i := range processes {
		config := childWorker(backend, path, fmt.Sprintf("process-%d", i), false, true)
		config.Delay = 40 * time.Millisecond
		processes[i] = startDistributedProcess(t, config)
		processes[i].waitMessage(t, "ready")
	}
	ids := make([]workflow.InstanceID, 18)
	for i := range ids {
		var err error
		ids[i], err = wf.Start(t.Context(), &distributedParams{Mode: distributedWork, Sleep: 0})
		require.NoError(t, err)
	}
	owners := make(map[string]bool)
	for _, id := range ids {
		out := distributedResult(t, wf, id)
		owners[out.CompletedBy] = true
		require.Equal(t, 1, distributedCalls(t, db, backend, id, "prepare"))
		require.Equal(t, 1, distributedCalls(t, db, backend, id, "finish"))
	}
	require.GreaterOrEqual(
		t,
		len(owners),
		2,
		"independent processes must actually execute the workload",
	)
}

func distributedPublicationSurvivesExitAndCrash(t *testing.T, backend, path string) {
	db, wf, inspector := newDistributedFixture(t, backend, path)
	id, err := wf.Start(
		t.Context(),
		&distributedParams{Mode: distributedWait, Sleep: 100 * time.Millisecond},
	)
	require.NoError(t, err)
	waiting := startDistributedProcess(t, childWorker(backend, path, "waiter", false, true))
	waiting.waitMessage(t, "ready")
	require.Eventually(t, func() bool {
		detail, err := inspector.Get(t.Context(), id)
		return err == nil && detail.Summary.Status == workflow.StatusWaiting
	}, 8*time.Second, 10*time.Millisecond)
	waiting.kill()
	publisher := startDistributedProcess(
		t,
		distributedChildConfig{
			Backend:    backend,
			Database:   path,
			Tag:        "publisher",
			Role:       distributedPublisher,
			HoldFinish: false,
			Heartbeat:  false,
			Delay:      0,
			Payload:    "approved",
		},
	)
	publisher.waitMessage(t, "published")
	<-publisher.finished
	require.NoError(t, publisher.waitErr)
	recovery := startDistributedProcess(t, childWorker(backend, path, "new-worker", false, true))
	recovery.waitMessage(t, "ready")
	out := distributedResult(t, wf, id)
	require.Equal(t, "approved", out.Approval)
	require.Equal(t, "waiter", out.PreparedBy)
	require.Equal(t, "new-worker", out.CompletedBy)
	require.Equal(t, 1, distributedCalls(t, db, backend, id, "prepare"))
	require.Equal(t, 1, distributedCalls(t, db, backend, id, "finish"))
}

func distributedPauseSurvivesReplacement(t *testing.T, backend, path, kind string) {
	db, wf, inspector := newDistributedFixture(t, backend, path)
	params := distributedParams{Mode: distributedWork, Sleep: 0}
	var opts []workflow.StartOption
	status := workflow.StatusSleeping
	if kind == "sleep" {
		params.Sleep = 800 * time.Millisecond
	} else {
		params.Mode = distributedRetry
		status = workflow.StatusRetrying
		opts = []workflow.StartOption{workflow.WithRetryStrategy(workflow.RetryStrategy{MaxAttempts: 3, BaseDelay: 800 * time.Millisecond, Factor: 1, MaxDelay: time.Second})}
	}
	id, err := wf.Start(t.Context(), &params, opts...)
	require.NoError(t, err)
	original := startDistributedProcess(t, childWorker(backend, path, "original", false, true))
	original.waitMessage(t, "ready")
	var deadline time.Time
	require.Eventually(t, func() bool {
		detail, err := inspector.Get(t.Context(), id)
		if err != nil || detail.Summary.Status != status || detail.Summary.NextRunAt == nil {
			return false
		}
		deadline = *detail.Summary.NextRunAt
		return true
	}, 8*time.Second, 10*time.Millisecond)
	original.kill()
	replacement := startDistributedProcess(
		t,
		childWorker(backend, path, "replacement", false, true),
	)
	replacement.waitMessage(t, "ready")
	require.True(
		t,
		time.Now().Before(deadline),
		"replacement must begin polling before the durable deadline",
	)
	out := distributedResult(t, wf, id)
	require.Equal(t, "replacement", out.CompletedBy)
	require.Equal(t, 1, distributedCalls(t, db, backend, id, "prepare"))
	var calledAt int64
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), distributedSQL(backend, `SELECT MIN(called_at_ns) FROM distributed_activity_calls WHERE instance_id=? AND step='finish' AND worker_tag='replacement'`), string(id)).
			Scan(&calledAt),
	)
	require.False(
		t,
		time.Unix(0, calledAt).Before(deadline),
		"replacement workers must not bypass a durable pause",
	)
	expected := 1
	if kind == distributedRetry {
		expected = 2
	}
	require.Equal(t, expected, distributedCalls(t, db, backend, id, "finish"))
}
