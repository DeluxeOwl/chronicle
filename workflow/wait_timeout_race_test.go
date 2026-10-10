package workflow_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

// waitTimeoutClock pauses a selected clock read without blocking publishers.
// The resumed read observes advances made while it was paused.
type waitTimeoutClock struct {
	mu          sync.Mutex
	now         time.Time
	pauseAfter  int
	paused      chan struct{}
	release     chan struct{}
	sampleFirst bool
}

func newWaitTimeoutClock() *waitTimeoutClock {
	return &waitTimeoutClock{
		mu:          sync.Mutex{},
		now:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		pauseAfter:  0,
		paused:      nil,
		release:     nil,
		sampleFirst: false,
	}
}

func (c *waitTimeoutClock) Now() time.Time {
	c.mu.Lock()
	sampled := c.now
	sampleFirst := c.sampleFirst
	var paused, release chan struct{}
	if c.pauseAfter > 0 {
		c.pauseAfter--
		if c.pauseAfter == 0 {
			paused, release = c.paused, c.release
		}
	}
	c.mu.Unlock()
	if paused != nil {
		close(paused)
		<-release
		if sampleFirst {
			return sampled
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *waitTimeoutClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

func (c *waitTimeoutClock) PauseAfter(reads int) (<-chan struct{}, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pauseAfter = reads
	c.paused = make(chan struct{})
	c.release = make(chan struct{})
	release := c.release
	var once sync.Once
	return c.paused, func() { once.Do(func() { close(release) }) }
}

func waitTimeoutSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("workflow did not reach the controlled deadline race")
	}
}

func newWaitTimeoutRunner(
	t *testing.T,
	persistent bool,
	clock *waitTimeoutClock,
) (*workflow.Runner, *workflow.Inspector) {
	t.Helper()
	if !persistent {
		log := eventlog.NewMemory()
		runner, err := workflow.NewRunner(log, nil, workflow.WithNowFunc(clock.Now))
		require.NoError(t, err)
		inspector, err := workflow.NewInspector(log)
		require.NoError(t, err)
		return runner, inspector
	}
	db := openWaitTimeoutDB(t, 1)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	log, err := eventlog.NewSqlite(db)
	require.NoError(t, err)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)
	return runner, inspector
}

func newWaitTimeoutWorkflow(runner *workflow.Runner) *workflow.Workflow[struct{}, string] {
	return workflow.New(
		runner,
		"wait-timeout-race",
		func(wctx *workflow.Context, _ *struct{}) (*string, error) {
			payload, err := workflow.WaitForEvent[string](
				wctx,
				"approval:timeout-race",
				workflow.WaitForEventOptions{Timeout: time.Hour},
			)
			if err != nil {
				return nil, err
			}
			return &payload, nil
		},
	)
}

func openWaitTimeoutDB(t *testing.T, maxConnections int) *sql.DB {
	t.Helper()
	db, err := sql.Open(
		"sqlite3",
		filepath.Join(t.TempDir(), "waits.db")+"?_journal_mode=WAL&_busy_timeout=5000",
	)
	require.NoError(t, err)
	db.SetMaxOpenConns(maxConnections)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestWorkflow_OnTimePublicationDuringDeadlineCheck(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			clock := newWaitTimeoutClock()
			deadline := clock.Now().Add(time.Hour)
			runner, inspector := newWaitTimeoutRunner(t, backend == "sqlite", clock)
			wf := newWaitTimeoutWorkflow(runner)
			id, err := wf.Start(t.Context(), &struct{}{})
			require.NoError(t, err)
			_, err = wf.Run(t.Context(), id)
			require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
			before, err := inspector.Get(t.Context(), id)
			require.NoError(t, err)

			clock.Set(deadline.Add(-time.Second))
			// Run checks readiness, then reads time after finding no publication.
			paused, release := clock.PauseAfter(2)
			t.Cleanup(release)
			type runResult struct {
				output *string
				err    error
			}
			results := make(chan runResult, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				output, runErr := wf.Run(t.Context(), id)
				results <- runResult{output: output, err: runErr}
			}()
			t.Cleanup(func() { release(); waitTimeoutSignal(t, done) })
			waitTimeoutSignal(t, paused)
			require.NoError(
				t,
				workflow.PublishEvent(
					t.Context(),
					runner,
					"approval:timeout-race",
					"on-time-first",
				),
			)
			clock.Set(deadline.Add(time.Second))
			release()
			waitTimeoutSignal(t, done)
			result := <-results
			if result.err != nil {
				require.ErrorIs(t, result.err, workflow.ErrWorkflowInfrastructure)
				unchanged, inspectErr := inspector.Get(t.Context(), id)
				require.NoError(t, inspectErr)
				require.Equal(
					t,
					before.History,
					unchanged.History,
					"a timeout race must roll back every workflow event",
				)
				result.output, result.err = wf.Run(t.Context(), id)
			}
			require.NoError(t, result.err)
			require.NotNil(t, result.output)
			require.Equal(t, "on-time-first", *result.output)
			detail, err := inspector.Get(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, workflow.StatusCompleted, detail.Summary.Status)
			require.Equal(
				t,
				1,
				detail.Summary.Attempt,
				"publication races must not consume a workflow retry",
			)
			require.Len(t, detail.Steps, 1)
			require.Equal(t, string(workflow.StatusCompleted), detail.Steps[0].Status)
		})
	}
}

func TestWorkflow_SQLiteTimeoutCheckpointRechecksPublication(t *testing.T) {
	db := openWaitTimeoutDB(t, 2)
	clock, publisherClock := newWaitTimeoutClock(), newWaitTimeoutClock()
	deadline := clock.Now().Add(time.Hour)
	runner, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		db,
		workflow.WithNowFunc(clock.Now),
	)
	require.NoError(t, err)
	publisher, err := workflow.NewSqliteRunnerWithSyncQueue(
		t.Context(),
		db,
		workflow.WithNowFunc(publisherClock.Now),
	)
	require.NoError(t, err)
	log, err := eventlog.NewSqlite(db)
	require.NoError(t, err)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"handled-timeout-race",
		func(wctx *workflow.Context, _ *struct{}) (*string, error) {
			payload, waitErr := workflow.WaitForEvent[string](
				wctx,
				"approval:timeout-race",
				workflow.WaitForEventOptions{Timeout: time.Hour},
			)
			if errors.Is(waitErr, workflow.ErrEventTimeout) {
				fallback := "false-timeout"
				return &fallback, nil
			}
			if waitErr != nil {
				return nil, waitErr
			}
			return &payload, nil
		},
	)
	id, err := wf.Start(
		t.Context(),
		&struct{}{},
		workflow.WithRetryStrategy(workflow.DefaultRetryStrategy()),
	)
	require.NoError(t, err)
	_, err = wf.Run(t.Context(), id)
	require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)
	before, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)

	publisherClock.Set(deadline.Add(-time.Second))
	publisherClock.sampleFirst = true
	// Pause after sampling accepted-storage time while publication holds the
	// SQLite write lock. WAL readers cannot see this uncommitted publication.
	publicationPaused, releasePublication := publisherClock.PauseAfter(2)
	publicationDone := make(chan struct{})
	publicationErrors := make(chan error, 1)
	go func() {
		defer close(publicationDone)
		publicationErrors <- workflow.PublishEvent(t.Context(), publisher, "approval:timeout-race", "accepted-before-deadline")
	}()
	t.Cleanup(func() { releasePublication(); waitTimeoutSignal(t, publicationDone) })
	waitTimeoutSignal(t, publicationPaused)

	clock.Set(deadline.Add(time.Second))
	// Readiness, timeout decision, then the completion timestamp. The last
	// pause is after the boundary re-read and before the timeout append.
	checkpointPaused, releaseCheckpoint := clock.PauseAfter(3)
	runDone := make(chan struct{})
	runErrors := make(chan error, 1)
	go func() {
		defer close(runDone)
		_, runErr := wf.Run(t.Context(), id)
		runErrors <- runErr
	}()
	t.Cleanup(func() { releaseCheckpoint(); waitTimeoutSignal(t, runDone) })
	waitTimeoutSignal(t, checkpointPaused)
	publisherClock.Set(deadline.Add(time.Second))
	releasePublication()
	waitTimeoutSignal(t, publicationDone)
	require.NoError(t, <-publicationErrors)
	releaseCheckpoint()
	waitTimeoutSignal(t, runDone)
	require.ErrorIs(t, <-runErrors, workflow.ErrWorkflowInfrastructure)
	unchanged, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(
		t,
		before.History,
		unchanged.History,
		"the rejected timeout checkpoint must not persist",
	)
	require.Equal(t, workflow.StatusWaiting, unchanged.Summary.Status)
	require.Equal(t, 1, unchanged.Summary.Attempt)

	// A late repair publication must preserve the first payload and time.
	require.NoError(
		t,
		workflow.PublishEvent(t.Context(), publisher, "approval:timeout-race", "late-replacement"),
	)
	output, err := wf.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, "accepted-before-deadline", *output)
	detail, err := inspector.Get(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusCompleted, detail.Summary.Status)
	require.Equal(t, 1, detail.Summary.Attempt)
}

func TestWorkflow_SQLitePublicationUsesTimeAfterStorageWait(t *testing.T) {
	for _, boundary := range []string{"connection", "write_lock"} {
		t.Run(boundary, func(t *testing.T) {
			connections := 1
			if boundary == "write_lock" {
				connections = 2
			}
			db := openWaitTimeoutDB(t, connections)
			clock := newWaitTimeoutClock()
			deadline := clock.Now().Add(time.Hour)
			runner, err := workflow.NewSqliteRunnerWithSyncQueue(
				t.Context(),
				db,
				workflow.WithNowFunc(clock.Now),
			)
			require.NoError(t, err)
			wf := newWaitTimeoutWorkflow(runner)
			id, err := wf.Start(t.Context(), &struct{}{})
			require.NoError(t, err)
			_, err = wf.Run(t.Context(), id)
			require.ErrorIs(t, err, workflow.ErrWorkflowWaiting)

			var releaseStorage func()
			var once sync.Once
			if boundary == "connection" {
				conn, connErr := db.Conn(t.Context())
				require.NoError(t, connErr)
				releaseStorage = func() { once.Do(func() { _ = conn.Close() }) }
			} else {
				tx, txErr := db.BeginTx(t.Context(), nil)
				require.NoError(t, txErr)
				_, txErr = tx.ExecContext(t.Context(), `UPDATE workflow_ready_tasks SET claimed_by=claimed_by WHERE 0`)
				require.NoError(t, txErr)
				releaseStorage = func() { once.Do(func() { _ = tx.Rollback() }) }
			}
			t.Cleanup(releaseStorage)
			clock.Set(deadline.Add(-time.Second))
			clock.sampleFirst = true
			paused, releaseClock := clock.PauseAfter(1)
			done := make(chan struct{})
			publicationErrors := make(chan error, 1)
			waitCount := db.Stats().WaitCount
			go func() {
				defer close(done)
				publicationErrors <- workflow.PublishEvent(t.Context(), runner, "approval:timeout-race", "accepted-too-late")
			}()
			t.Cleanup(func() { releaseClock(); releaseStorage(); waitTimeoutSignal(t, done) })
			waitTimeoutSignal(t, paused)
			releaseClock()
			require.Eventually(t, func() bool {
				if boundary == "connection" {
					return db.Stats().WaitCount > waitCount
				}
				return db.Stats().InUse == 2
			}, 5*time.Second, time.Millisecond)
			clock.Set(deadline.Add(time.Second))
			releaseStorage()
			waitTimeoutSignal(t, done)
			require.NoError(t, <-publicationErrors)
			_, err = wf.Run(t.Context(), id)
			require.ErrorIs(
				t,
				err,
				workflow.ErrEventTimeout,
				"waiting for storage must not backdate publication",
			)
		})
	}
}
