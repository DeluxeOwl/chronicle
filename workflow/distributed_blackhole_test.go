//go:build !windows

package workflow_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

const distributedBlackholeTransactionTimeout = time.Second

// These observations establish the injected fault, not workflow persistence.
// Recovery and receipt fencing are checked through the public APIs below.
const (
	distributedBlackholeIdleSQL = `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity
		WHERE application_name = $1 AND state = 'idle in transaction'
	)`
	distributedBlackholeIdleWaitLockSQL = `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity AS owner JOIN pg_locks AS held ON held.pid = owner.pid
		WHERE owner.application_name = $1 AND owner.state = 'idle in transaction'
		AND held.locktype = 'advisory' AND held.granted
	)`
	distributedBlackholeBlockedWaitSQL = `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity AS blocked
		WHERE blocked.datname = current_database()
		AND blocked.wait_event_type = 'Lock' AND blocked.wait_event = 'advisory'
		AND EXISTS (
			SELECT 1 FROM pg_stat_activity AS owner
			WHERE owner.application_name = $1 AND owner.pid = ANY(pg_blocking_pids(blocked.pid))
		)
	)`
)

// The parent supplies an isolated database; these cases never start containers.
func distributedBlackholedTransactionsReleaseLocks(t *testing.T, backend, database string) {
	if backend != distributedPostgres {
		t.Skip("SQLite has no half-open PostgreSQL connections")
	}
	t.Run("task-claim-lock", func(t *testing.T) {
		distributedBlackholedClaimCanBeReclaimed(t, database)
	})
	t.Run("global-wait-lock", func(t *testing.T) {
		distributedBlackholedPublicationCannotStrandWaiters(t, database)
	})
}

func distributedBlackholedClaimCanBeReclaimed(t *testing.T, database string) {
	const lease = 100 * time.Millisecond
	const applicationName = "chronicle-blackholed-claim"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	healthyDB := openDistributedDB(t, distributedPostgres, database)
	proxied, proxy := startDistributedBlackholeProxy(t, database, applicationName)
	faultyDB := openDistributedDB(t, distributedPostgres, proxied)
	healthy, err := workflow.NewPostgresQueue(
		ctx,
		healthyDB,
		workflow.WithPostgresQueueLeaseDuration(lease),
	)
	require.NoError(t, err)
	faulty, err := workflow.NewPostgresQueue(ctx, faultyDB,
		workflow.WithPostgresQueueLeaseDuration(lease),
		workflow.WithPostgresQueueTransactionTimeout(distributedBlackholeTransactionTimeout),
	)
	require.NoError(t, err)
	id := workflow.InstanceID("blackholed-claim")
	require.NoError(
		t,
		healthy.Enqueue(ctx, workflow.QueuedTask{InstanceID: id, WorkflowName: "blackholed-claim"}),
	)
	claim, err := healthy.PollClaim(ctx)
	require.NoError(t, err)
	require.NotNil(t, claim)
	proxy.arm(string(claim.Receipt))
	interrupted := startDistributedBlackholeCall(t, func(ctx context.Context) error {
		return faulty.ExtendClaim(ctx, *claim, time.Second)
	})
	trap := distributedBlackholeAwait(t, ctx, proxy.trapped, "claim row response was not trapped")

	// The receipt row was locked, but its response never reached the caller.
	// Expiry alone cannot make SKIP LOCKED select this still-locked task.
	timer := time.NewTimer(2 * lease)
	defer timer.Stop()
	distributedBlackholeAwait(t, ctx, timer.C, "lease did not expire")
	distributedBlackholeAwaitBackend(
		t,
		ctx,
		healthyDB,
		distributedBlackholeIdleSQL,
		applicationName,
	)
	unavailable, err := healthy.PollClaim(ctx)
	require.NoError(t, err)
	require.Nil(
		t,
		unavailable,
		"an expired task must still be unavailable while its transaction holds the row lock",
	)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var reclaimed *workflow.ClaimedTask
	for reclaimed == nil {
		reclaimed, err = healthy.PollClaim(ctx)
		require.NoError(t, err)
		if reclaimed == nil {
			distributedBlackholeAwait(
				t,
				ctx,
				ticker.C,
				"half-open claim transaction prevented recovery",
			)
		}
	}
	require.Equal(t, id, reclaimed.InstanceID)
	require.NotEqual(t, claim.Receipt, reclaimed.Receipt)
	require.NoError(t, healthy.ExtendClaim(ctx, *reclaimed, 3*time.Second))

	// Client cancellation cannot free the upstream connection in this proxy.
	// PostgreSQL itself must terminate the idle transaction, while the original
	// caller must return an error without receiving even the server's EOF.
	distributedBlackholeAwait(
		t,
		ctx,
		trap.idleTimeout,
		"backend did not enforce its idle transaction timeout",
	)
	distributedBlackholeAwait(
		t,
		ctx,
		trap.serverClosed,
		"backend did not close its timed-out transaction",
	)
	interruptedErr := distributedBlackholeAwait(
		t,
		ctx,
		interrupted,
		"claim operation had no bounded client deadline",
	)
	require.Error(t, interruptedErr)
	require.NotErrorIs(
		t,
		interruptedErr,
		workflow.ErrStaleClaim,
		"a withheld storage response is not a receipt rejection",
	)
	require.ErrorIs(t, healthy.CompleteClaim(ctx, *claim), workflow.ErrStaleClaim)
	require.ErrorIs(t, healthy.ExtendClaim(ctx, *claim, time.Second), workflow.ErrStaleClaim)
	require.NoError(t, healthy.CompleteClaim(ctx, *reclaimed))
	remaining, err := healthy.PollClaim(ctx)
	require.NoError(t, err)
	require.Nil(t, remaining)
}

func distributedBlackholedPublicationCannotStrandWaiters(t *testing.T, database string) {
	const applicationName = "chronicle-blackholed-publication"
	const eventName = "distributed:blackholed-publication"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	healthyDB := openDistributedDB(t, distributedPostgres, database)
	proxied, proxy := startDistributedBlackholeProxy(t, database, applicationName)
	faultyDB := openDistributedDB(t, distributedPostgres, proxied)
	healthy, err := newDistributedRunner(ctx, healthyDB, distributedPostgres)
	require.NoError(t, err)
	faulty, err := newDistributedRunner(
		ctx,
		faultyDB,
		distributedPostgres,
		workflow.WithPostgresQueueOpts(
			workflow.WithPostgresQueueTransactionTimeout(distributedBlackholeTransactionTimeout),
		),
	)
	require.NoError(t, err)
	wf := workflow.New(
		healthy,
		"blackholed-publication-waiter",
		func(wctx *workflow.Context, _ *struct{}) (*string, error) {
			payload, err := workflow.WaitForEvent[string](
				wctx,
				eventName,
				workflow.WaitForEventOptions{Name: "Await healthy publication", Timeout: 0},
			)
			if err != nil {
				return nil, err
			}
			return &payload, nil
		},
	)
	id, err := wf.Start(ctx, &struct{}{})
	require.NoError(t, err)
	_, err = wf.Run(ctx, id)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	status, err := wf.GetStatus(ctx, id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusWaiting, status.Status)

	// Arm only after both constructors and wait registration: SHOW follows
	// acquisition of the global wait advisory lock in the publication API.
	proxy.arm("read committed")
	interrupted := startDistributedBlackholeCall(t, func(ctx context.Context) error {
		return workflow.PublishEvent(ctx, faulty, eventName, "withheld")
	})
	trap := distributedBlackholeAwait(
		t,
		ctx,
		proxy.trapped,
		"publication isolation response was not trapped",
	)
	distributedBlackholeAwaitBackend(
		t,
		ctx,
		healthyDB,
		distributedBlackholeIdleWaitLockSQL,
		applicationName,
	)
	independent := startDistributedBlackholeCall(t, func(ctx context.Context) error {
		return workflow.PublishEvent(
			ctx,
			healthy,
			"distributed:unrelated-blackhole-publication",
			"unrelated",
		)
	})
	distributedBlackholeAwaitBackend(
		t,
		ctx,
		healthyDB,
		distributedBlackholeBlockedWaitSQL,
		applicationName,
	)
	distributedBlackholeAwait(
		t,
		ctx,
		trap.idleTimeout,
		"backend retained the global wait lock without an idle timeout",
	)
	distributedBlackholeAwait(
		t,
		ctx,
		trap.serverClosed,
		"backend did not close its timed-out publication",
	)
	require.NoError(
		t,
		distributedBlackholeAwait(
			t,
			ctx,
			independent,
			"half-open publication blocked an unrelated healthy publisher",
		),
	)
	interruptedErr := distributedBlackholeAwait(
		t,
		ctx,
		interrupted,
		"publication had no bounded client deadline",
	)
	require.ErrorIs(t, interruptedErr, workflow.ErrWorkflowInfrastructure)

	// The failed publication must not consume first-write-wins. A healthy
	// publication wakes the real waiter, and a duplicate cannot replace it.
	require.NoError(t, workflow.PublishEvent(ctx, healthy, eventName, "healthy"))
	require.NoError(t, workflow.PublishEvent(ctx, healthy, eventName, "later"))
	out, err := wf.Run(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Equal(t, "healthy", *out)
	status, err = wf.GetStatus(ctx, id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCompleted, status.Status)
	require.Equal(t, 1, status.Attempt)
}

// Calls deliberately have cancellation but no caller deadline. Otherwise a
// deadline supplied by the test could hide a missing queue transaction bound.
func startDistributedBlackholeCall(t *testing.T, call func(context.Context) error) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		timer := time.NewTimer(time.Second)
		defer timer.Stop()
		select {
		case <-finished:
		case <-timer.C:
			t.Error("blackholed API call did not stop after cleanup cancellation")
		}
	})
	go func() {
		defer close(finished)
		result <- call(ctx)
	}()
	return result
}

func distributedBlackholeAwait[T any](
	t *testing.T,
	ctx context.Context,
	result <-chan T,
	message string,
) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-ctx.Done():
		t.Fatalf("%s: %v", message, ctx.Err())
		var zero T
		return zero
	}
}

func distributedBlackholeAwaitBackend(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	query, applicationName string,
) {
	t.Helper()
	observationCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var observed bool
		err := db.QueryRowContext(observationCtx, query, applicationName).Scan(&observed)
		require.NoError(t, err, "could not observe the injected lock fault for %s", applicationName)
		if observed {
			return
		}
		distributedBlackholeAwait(
			t,
			observationCtx,
			ticker.C,
			"backend never entered the required lock state: "+applicationName,
		)
	}
}
