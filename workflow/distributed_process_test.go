//go:build !windows

package workflow_test

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	_ "github.com/jackc/pgx/v5/stdlib" // Register the driver in both parent and child test processes.
	"github.com/stretchr/testify/require"
)

const distributedChildEnv = "CHRONICLE_DISTRIBUTED_WORKFLOW_CHILD"

const (
	distributedSQLite    = "sqlite"
	distributedPostgres  = "postgres"
	distributedWork      = "work"
	distributedWait      = "wait"
	distributedRetry     = "retry"
	distributedWorker    = "worker"
	distributedPublisher = "publisher"
	distributedCanceller = "canceller"
	distributedStarter   = "starter"
	distributedEventName = "distributed:approval"
	distributedLease     = 600 * time.Millisecond
)

type distributedParams struct {
	Mode      string        `json:"mode"`
	Sleep     time.Duration `json:"sleep"`
	EventName string        `json:"eventName" exhaustruct:"optional"`
}

type distributedOutput struct {
	PreparedBy  string `json:"preparedBy"`
	CompletedBy string `json:"completedBy"`
	Approval    string `json:"approval"`
}

type distributedChildConfig struct {
	Backend       string              `json:"backend"`
	Database      string              `json:"database"`
	Tag           string              `json:"tag"`
	Role          string              `json:"role"`
	HoldFinish    bool                `json:"holdFinish"`
	Heartbeat     bool                `json:"heartbeat"`
	Delay         time.Duration       `json:"delay"`
	Payload       string              `json:"payload"`
	ClockOffset   time.Duration       `json:"clockOffset" exhaustruct:"optional"`
	LeaseDuration time.Duration       `json:"leaseDuration" exhaustruct:"optional"`
	InstanceID    workflow.InstanceID `json:"instanceID" exhaustruct:"optional"`
	Publications  []string            `json:"publications" exhaustruct:"optional"`
}

type distributedMessage struct {
	Type       string              `json:"type"`
	InstanceID workflow.InstanceID `json:"instanceID"`
	Step       string              `json:"step"`
	Stale      bool                `json:"stale"`
	Error      string              `json:"error"`
}

// TestDistributedWorkflowChild is re-executed by the parent tests. Each child
// has independent queue, registry, event log, connection pool, and clock state.
func TestDistributedWorkflowChild(t *testing.T) {
	configJSON := os.Getenv(distributedChildEnv)
	if configJSON == "" {
		t.Skip("subprocess entry point")
	}
	var config distributedChildConfig
	require.NoError(t, json.Unmarshal([]byte(configJSON), &config))
	ctx, stop := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var outputMu sync.Mutex
	emit := func(message distributedMessage) error {
		outputMu.Lock()
		defer outputMu.Unlock()
		return json.NewEncoder(os.Stdout).Encode(message)
	}
	if config.Role == distributedStarter {
		require.NoError(
			t,
			emit(
				distributedMessage{
					Type:       "ready",
					InstanceID: "",
					Step:       "",
					Stale:      false,
					Error:      "",
				},
			),
		)
		var command string
		_, err := fmt.Fscanln(os.Stdin, &command)
		require.NoError(t, err)
	}
	db := openDistributedDB(t, config.Backend, config.Database)
	runner, err := newDistributedRunnerWithLease(
		ctx, db, config.Backend, config.LeaseDuration,
		workflow.WithNowFunc(func() time.Time { return time.Now().Add(config.ClockOffset) }),
	)
	require.NoError(t, err)
	if config.Role == distributedPublisher {
		require.NoError(t, runDistributedPublisher(ctx, runner, config, emit))
		return
	}
	if config.Role == distributedStarter {
		definition := workflow.New(
			runner,
			"concurrent-start",
			func(*workflow.Context, *struct{}) (*struct{}, error) { return &struct{}{}, nil },
		)
		id, err := definition.Start(ctx, &struct{}{})
		require.NoError(t, err)
		require.NoError(
			t,
			emit(
				distributedMessage{
					Type:       "started",
					InstanceID: id,
					Step:       "",
					Stale:      false,
					Error:      "",
				},
			),
		)
		return
	}
	if config.Role == distributedCanceller {
		require.NoError(
			t,
			workflow.CancelWorkflow(
				ctx,
				runner,
				config.InstanceID,
				"cancelled by independent process",
			),
		)
		require.NoError(
			t,
			emit(
				distributedMessage{
					Type:       "cancelled",
					InstanceID: config.InstanceID,
					Step:       "",
					Stale:      false,
					Error:      "",
				},
			),
		)
		return
	}
	workflow.New(
		runner,
		"distributed-workflow",
		func(wctx *workflow.Context, params *distributedParams) (*distributedOutput, error) {
			return runDistributedWorkflow(wctx, params, db, config, emit)
		},
	)
	require.NoError(
		t,
		emit(distributedMessage{Type: "ready", InstanceID: "", Step: "", Stale: false, Error: ""}),
	)
	heartbeat := time.Duration(-1)
	if config.Heartbeat {
		heartbeat = 100 * time.Millisecond
	}
	concurrency := 2
	if config.HoldFinish {
		// A stalled test worker must not reclaim its own expired task.
		concurrency = 1
	}
	err = runner.RunWorkers(
		ctx,
		concurrency,
		workflow.WorkerOptions{PollInterval: 10 * time.Millisecond, LeaseExtendInterval: heartbeat},
	)
	if !errors.Is(err, context.Canceled) {
		require.NoError(t, err)
	}
}

func newDistributedRunner(
	ctx context.Context,
	db *sql.DB,
	backend string,
	opts ...workflow.RunnerOption,
) (*workflow.Runner, error) {
	return newDistributedRunnerWithLease(ctx, db, backend, distributedLease, opts...)
}

func newDistributedRunnerWithLease(
	ctx context.Context,
	db *sql.DB,
	backend string,
	lease time.Duration,
	opts ...workflow.RunnerOption,
) (*workflow.Runner, error) {
	if lease == 0 {
		lease = distributedLease
	}
	if backend == distributedPostgres {
		opts = append(opts, workflow.WithPostgresQueueOpts(
			workflow.WithPostgresQueueLeaseDuration(lease),
			workflow.WithPostgresQueueWorkerID("shared-process-label"),
		))
		return workflow.NewPostgresRunnerWithSyncQueue(ctx, db, opts...)
	}
	opts = append(opts, workflow.WithSyncQueueOpts(
		workflow.WithSyncQueueLeaseDuration(lease),
		workflow.WithSyncQueueWorkerID("shared-process-label"),
	))
	return workflow.NewSqliteRunnerWithSyncQueue(ctx, db, opts...)
}

func runDistributedPublisher(
	ctx context.Context,
	runner *workflow.Runner,
	config distributedChildConfig,
	emit func(distributedMessage) error,
) error {
	names := config.Publications
	if len(names) == 0 {
		names = []string{distributedEventName}
	} else {
		if err := emit(distributedMessage{Type: "ready", InstanceID: "", Step: "", Stale: false, Error: ""}); err != nil {
			return err
		}
		var command string
		if _, err := fmt.Fscanln(os.Stdin, &command); err != nil {
			return err
		}
	}
	for _, name := range names {
		if err := workflow.PublishEvent(ctx, runner, name, config.Payload); err != nil {
			return err
		}
	}
	return emit(
		distributedMessage{Type: "published", InstanceID: "", Step: "", Stale: false, Error: ""},
	)
}

func runDistributedWorkflow(
	wctx *workflow.Context,
	params *distributedParams,
	db *sql.DB,
	config distributedChildConfig,
	emit func(distributedMessage) error,
) (*distributedOutput, error) {
	prepared, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
		if _, err := recordDistributedCall(ctx, db, config.Backend, wctx.InstanceID(), "prepare", config.Tag); err != nil {
			return "", err
		}
		return config.Tag, nil
	}, workflow.WithStepName("Prepare"))
	if err != nil {
		return nil, err
	}
	if params.Sleep > 0 {
		if err := workflow.Sleep(wctx, params.Sleep, workflow.WithStepName("Durable pause")); err != nil {
			return nil, err
		}
	}
	var approval string
	if params.Mode == distributedWait {
		name := params.EventName
		if name == "" {
			name = distributedEventName
		}
		approval, err = workflow.WaitForEvent[string](
			wctx,
			name,
			workflow.WaitForEventOptions{Name: "Await approval", Timeout: 0},
		)
		if err != nil {
			return nil, err
		}
	}
	// Unnamed deliberately: the test must fence a stale completion even
	// when another claimant has not appended a step-start event.
	completed, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
		return finishDistributedStep(ctx, params, db, config, wctx.InstanceID(), emit)
	})
	message := distributedMessage{
		Type:       "returned",
		InstanceID: wctx.InstanceID(),
		Step:       "finish",
		Stale:      errors.Is(err, workflow.ErrStaleClaim),
		Error:      "",
	}
	if err != nil {
		message.Error = err.Error()
	}
	if emitErr := emit(message); emitErr != nil {
		return nil, errors.Join(err, emitErr)
	}
	if err != nil {
		return nil, err
	}
	return &distributedOutput{PreparedBy: prepared, CompletedBy: completed, Approval: approval}, nil
}

func finishDistributedStep(
	ctx context.Context,
	params *distributedParams,
	db *sql.DB,
	config distributedChildConfig,
	id workflow.InstanceID,
	emit func(distributedMessage) error,
) (string, error) {
	attempt, err := recordDistributedCall(ctx, db, config.Backend, id, "finish", config.Tag)
	if err != nil {
		return "", err
	}
	if err := emit(distributedMessage{Type: "entered", InstanceID: id, Step: "finish", Stale: false, Error: ""}); err != nil {
		return "", err
	}
	if config.HoldFinish {
		// Blocking on stdin deliberately ignores cancellation: this models a
		// stalled activity that can return after another process takes ownership.
		var command string
		if _, err := fmt.Fscanln(os.Stdin, &command); err != nil {
			return "", err
		}
		if command != "release" {
			return "", errors.New("unexpected subprocess command")
		}
	}
	if config.Delay > 0 {
		timer := time.NewTimer(config.Delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	if params.Mode == distributedRetry && attempt == 1 {
		return "", errors.New("transient activity failure")
	}
	return config.Tag, nil
}

func openDistributedDB(t *testing.T, backend, database string) *sql.DB {
	t.Helper()
	driver := "sqlite3"
	if backend == distributedPostgres {
		driver = "pgx"
	} else {
		database += "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	}
	db, err := sql.Open(driver, database)
	require.NoError(t, err)
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func distributedSQL(backend, query string) string {
	if backend != distributedPostgres {
		return query
	}
	index := 0
	var out strings.Builder
	for _, piece := range strings.SplitAfter(query, "?") {
		if strings.HasSuffix(piece, "?") {
			index++
			out.WriteString(strings.TrimSuffix(piece, "?"))
			fmt.Fprintf(&out, "$%d", index)
		} else {
			out.WriteString(piece)
		}
	}
	return out.String()
}

// This table is an external activity sink, not Chronicle's private state.
// Counts measure visible effects that a crashed activity can repeat.
func recordDistributedCall(
	ctx context.Context,
	db *sql.DB,
	backend string,
	id workflow.InstanceID,
	step, tag string,
) (int, error) {
	_, err := db.ExecContext(
		ctx,
		distributedSQL(
			backend,
			`INSERT INTO distributed_activity_calls(instance_id,step,worker_tag,called_at_ns) VALUES(?,?,?,?)`,
		),
		string(id),
		step,
		tag,
		time.Now().UnixNano(),
	)
	if err != nil {
		return 0, err
	}
	var attempts int
	err = db.QueryRowContext(ctx, distributedSQL(backend, `SELECT COUNT(*) FROM distributed_activity_calls WHERE instance_id=? AND step=?`), string(id), step).
		Scan(&attempts)
	return attempts, err
}

type distributedProcess struct {
	cmd        *exec.Cmd
	input      io.WriteCloser
	messages   chan distributedMessage
	finished   chan struct{}
	waitErr    error
	stderrPath string
	mu         sync.Mutex
	output     strings.Builder
}

func startDistributedProcess(t *testing.T, config distributedChildConfig) *distributedProcess {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	data, err := json.Marshal(config)
	require.NoError(t, err)
	ctx := t.Context()
	cmd := exec.CommandContext(
		ctx,
		binary,
		"-test.run=^TestDistributedWorkflowChild$",
		"-test.count=1",
		"-test.timeout=30s",
	)
	cmd.Env = append(os.Environ(), distributedChildEnv+"="+string(data), "GORACE=atexit_sleep_ms=0")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	input, err := cmd.StdinPipe()
	require.NoError(t, err)
	stderr, err := os.Create(filepath.Join(t.TempDir(), "child-stderr.log"))
	require.NoError(t, err)
	cmd.Stderr = stderr
	t.Cleanup(func() { _ = stderr.Close(); _ = input.Close() })
	process := &distributedProcess{
		cmd:        cmd,
		input:      input,
		messages:   make(chan distributedMessage, 128),
		finished:   make(chan struct{}),
		waitErr:    nil,
		stderrPath: stderr.Name(),
		mu:         sync.Mutex{},
		output:     strings.Builder{},
	}
	require.NoError(t, cmd.Start())
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			process.mu.Lock()
			process.output.WriteString(line + "\n")
			process.mu.Unlock()
			var message distributedMessage
			if json.Unmarshal([]byte(line), &message) == nil {
				select {
				case process.messages <- message:
				case <-ctx.Done():
				}
			}
		}
		process.waitErr = errors.Join(scanner.Err(), cmd.Wait())
		close(process.finished)
	}()
	t.Cleanup(process.kill)
	return process
}

func (p *distributedProcess) kill() {
	select {
	case <-p.finished:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.finished
}

func (p *distributedProcess) log() string {
	p.mu.Lock()
	out := p.output.String()
	p.mu.Unlock()
	stderr, err := os.ReadFile(p.stderrPath)
	if err == nil {
		out += "\nstderr:\n" + string(stderr)
	}
	return out
}

func (p *distributedProcess) waitMessage(t *testing.T, kind string) distributedMessage {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-p.messages:
			if message.Type == kind {
				return message
			}
		case <-p.finished:
			// Publication children can exit immediately after emitting the
			// expected message. Drain buffered messages before reporting exit.
			for {
				select {
				case message := <-p.messages:
					if message.Type == kind {
						return message
					}
				default:
					t.Fatalf("child exited while waiting for %s: %v\n%s", kind, p.waitErr, p.log())
				}
			}
		case <-timer.C:
			t.Fatalf("child did not report %s\n%s", kind, p.log())
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
}

func (p *distributedProcess) release(t *testing.T) {
	t.Helper()
	_, err := fmt.Fprintln(p.input, "release")
	require.NoError(t, err)
}

func newDistributedFixture(
	t *testing.T,
	backend, database string,
) (*sql.DB, *workflow.Workflow[distributedParams, distributedOutput], *workflow.Inspector) {
	t.Helper()
	db := openDistributedDB(t, backend, database)
	definition := `CREATE TABLE distributed_activity_calls(id INTEGER PRIMARY KEY,instance_id TEXT NOT NULL,step TEXT NOT NULL,worker_tag TEXT NOT NULL,called_at_ns INTEGER NOT NULL)`
	if backend == distributedPostgres {
		definition = `CREATE TABLE distributed_activity_calls(id BIGSERIAL PRIMARY KEY,instance_id TEXT NOT NULL,step TEXT NOT NULL,worker_tag TEXT NOT NULL,called_at_ns BIGINT NOT NULL)`
	}
	_, err := db.ExecContext(t.Context(), definition)
	require.NoError(t, err)
	runner, err := newDistributedRunner(t.Context(), db, backend)
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"distributed-workflow",
		func(*workflow.Context, *distributedParams) (*distributedOutput, error) {
			return nil, errors.New("parent must not execute workflow callbacks")
		},
	)
	var log event.GlobalLog
	if backend == distributedPostgres {
		log, err = eventlog.NewPostgres(db)
	} else {
		log, err = eventlog.NewSqlite(db)
	}
	require.NoError(t, err)
	inspector, err := workflow.NewInspector(log)
	require.NoError(t, err)
	return db, wf, inspector
}

func childWorker(backend, database, tag string, hold, heartbeat bool) distributedChildConfig {
	return distributedChildConfig{
		Backend:    backend,
		Database:   database,
		Tag:        tag,
		Role:       distributedWorker,
		HoldFinish: hold,
		Heartbeat:  heartbeat,
		Delay:      0,
		Payload:    "",
	}
}

func distributedResult(
	t *testing.T,
	wf *workflow.Workflow[distributedParams, distributedOutput],
	id workflow.InstanceID,
) *distributedOutput {
	t.Helper()
	var result *distributedOutput
	require.Eventually(
		t,
		func() bool { var err error; result, err = wf.GetResult(t.Context(), id); return err == nil },
		8*time.Second,
		10*time.Millisecond,
	)
	return result
}

func distributedCalls(
	t *testing.T,
	db *sql.DB,
	backend string,
	id workflow.InstanceID,
	step string,
) int {
	t.Helper()
	var count int
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), distributedSQL(backend, `SELECT COUNT(*) FROM distributed_activity_calls WHERE instance_id=? AND step=?`), string(id), step).
			Scan(&count),
	)
	return count
}
