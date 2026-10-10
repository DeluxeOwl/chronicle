# Durable workflows

A workflow is a Go function with persisted checkpoints. Each instance has its own event stream.
On recovery, Chronicle replays completed steps and runs the remaining work.

## Run workers with SQLite

Use the transactional runner for durable task scheduling:

```go
runner, err := workflow.NewSqliteRunnerWithSyncQueue(ctx, db)
if err != nil {
    return err
}

wf := workflow.New(runner, "send-welcome-email",
    func(wctx *workflow.Context, params *User) (*Receipt, error) {
        receipt, err := workflow.Step(wctx,
            func(ctx context.Context) (Receipt, error) {
                key := string(wctx.InstanceID()) + ":send-welcome-email"
                return emailClient.Send(ctx, params.Email, key)
            },
            workflow.WithStepName("Send welcome email"),
        )
        if err != nil {
            return nil, err
        }
        return &receipt, nil
    },
)

id, err := wf.Start(ctx, &User{Email: "alice@example.com"})
if err != nil {
    return err
}
_ = id // Keep this ID to read the result or cancel the instance.

// Blocks until cancellation. Register all workflows before starting workers.
return runner.RunWorkers(ctx, 4, workflow.WorkerOptions{
    PollInterval:        200 * time.Millisecond,
    LeaseExtendInterval: 10 * time.Second,
})
```

`RunWorker` runs one worker. `RunWorkers` bounds concurrent executions without prefetching tasks beyond available execution slots.
Neither method starts an HTTP server.

Workers automatically renew leases. Transient queue and storage errors retain work for another attempt.
An unknown workflow definition does not delete its task.

On cancellation, workers interrupt their callbacks, stop their heartbeats, release unfinished claims, and join all goroutines.
Callbacks and custom queues must honor their context. A callback that ignores cancellation can prevent shutdown.

### SQLite configuration

Use a file-backed database for persistence. Configure a busy timeout and WAL mode for competing connections or processes:

```go
db, err := sql.Open("sqlite3", "workflows.db?_journal_mode=WAL&_busy_timeout=10000")
```

`NewSqliteRunnerWithSyncQueue` writes events and task changes in the same transaction.
Published events and their wake-up tasks also commit together.
Queue constructors add required columns to older tables without deleting existing tasks.
All processes sharing the database must use synchronized clocks.

## Run workers on separate hosts

Use PostgreSQL when workers run on different machines. Each process opens its own connection pool and registers compatible workflow definitions.

Authoritative event history and task scheduling share the same database transaction:

```go
import _ "github.com/jackc/pgx/v5/stdlib"

db, err := sql.Open("pgx", dsn)
if err != nil {
    return err
}
runner, err := workflow.NewPostgresRunnerWithSyncQueue(ctx, db,
    workflow.WithPostgresQueueOpts(
        workflow.WithPostgresQueueLeaseDuration(30*time.Second),
        workflow.WithPostgresQueueWorkerID("worker-east-1"),
        workflow.WithPostgresQueueTransactionTimeout(5*time.Second),
    ),
)
if err != nil {
    return err
}
// Register workflows with workflow.New before this call.
return runner.RunWorkers(ctx, 8, workflow.WorkerOptions{})
```

The worker ID is a diagnostic label. A unique receipt identifies each claim, even when processes use the same label.
Postgres claims use `FOR UPDATE SKIP LOCKED`. Lease checks use the database clock after task-lock acquisition.
Workflow checkpoints validate the claim inside the event-save transaction. An expired worker cannot save a result against an unchanged event version.

Workflow deadlines still use the runner clock. Keep workflow clocks synchronized, even though Postgres leases use database time.
Wait transactions require the default `READ COMMITTED` isolation level.
A shared advisory lock orders wait registration, event publication, and timeout decisions. Publication and waiter wake-up commit together.

Short storage transactions have a five-second default limit, configured with `WithPostgresQueueTransactionTimeout`.
The limit does not apply to activity callbacks. Client deadlines and transaction-local Postgres timeouts prevent half-open connections from retaining workflow locks indefinitely.
Server limits apply before event appends, publication, or explicit queue transactions acquire their locks.
`PollClaim` uses a single autocommit statement with a client deadline, not the transaction-local server limits.
Custom repositories must provide equivalent limits if they use `PostgresQueue` without the supplied runner.

## Execution contract

**Steps are at least once, not exactly once.** A process can stop after an external effect but before its checkpoint commits.
The next worker repeats that step. Use a stable idempotency key for external effects.
`wctx.InstanceID()` remains stable across retries and restarts.

Keep workflow code deterministic between checkpoints. Put network calls, clock reads, random choices, and external effects inside steps.
Step results persist as JSON. Step order is positional and must remain compatible across deployments.

A completed step returns its stored result without calling its function again.
Named steps also record their start time before the callback runs.
Changing a recorded step name or kind returns `ErrWorkflowInfrastructure` instead of executing incompatible work.
Names improve inspection. They do not replace positional step identity.

`Step2` is the error-only form of `Step`. Both accept `WithStepName`.
Unnamed successful steps retain completion-only tracing for compatibility.

### Handled step failures

A workflow can handle a domain step error and continue to a fallback checkpoint.
New failure events preserve that outcome, so recovery does not repeat the failed callback and change the recorded path.
An explicit workflow retry clears the failure of the propagated step. Earlier handled outcomes remain in history.

Live step errors wrap the original cause. Use `errors.Is` or `errors.As`, not direct equality.
On replay, the error wraps `*RecordedStepError` with the stored message.
History cannot restore the original Go type, wrapped chain, or sentinel identity.
For branches that depend on `errors.Is`, `errors.As`, or error codes, classify the error inside the step.
Return the classification as a successful, structured JSON result.

Legacy failure events lack the replay flag and retain their previous retry behavior.
Do not mix older and newer worker binaries for workflows that checkpoint after handled failures.

### Durable pauses

```go
if err := workflow.Sleep(wctx, 5*time.Minute,
    workflow.WithStepName("Cooling period")); err != nil {
    return nil, err
}

shipment, err := workflow.WaitForEvent[Shipment](wctx, "shipment:order-42",
    workflow.WaitForEventOptions{
        Timeout: time.Hour,
        Name:    "Await carrier",
    },
)
```

Sleep, event waits, and retry delays persist their scheduling intent and release the worker slot.
They do not block a goroutine until their deadline.
A zero-duration sleep completes in the same execution.
Direct `Run` calls also honor persisted sleep and retry deadlines.

`PublishEvent` delivers a named event to workflows waiting for that name:

```go
err := workflow.PublishEvent(ctx, runner, "shipment:order-42", Shipment{
    TrackingNumber: "TRACK-42",
})
```

The first publication wins. Use an occurrence-specific event name, not a recurring topic name.
A pre-published event resolves during the first wait call.
An event published before the deadline remains valid even if a worker resumes after the deadline.
An event published at or after the deadline does not resolve that wait.
Durable stores record publication time after they acquire the publication lock. This is accepted-storage time, not physical transaction commit time.
SQLite uses the runner clock. Postgres uses the database clock. Workflow deadlines must agree with that clock.
Timeout checkpoints recheck publication in their event-save transaction. A conflicting on-time publication causes recoverable rollback, not a domain failure.
Legacy publications without timestamps remain deliverable because their original timing is unknown.

### Retries

```go
id, err := wf.Start(ctx, params,
    workflow.WithRetryStrategy(workflow.DefaultRetryStrategy()),
)
```

Retries resume from completed checkpoints. The step whose error propagates from the workflow executes again.
A cleanup checkpoint does not hide that propagated failure. An unrelated later error does not erase an earlier handled outcome.
The default strategy allows five total attempts, with exponential backoff from one second to five minutes.
`MaxAttempts: 0` also means five total attempts. No retry strategy means no automatic domain retry.

An unhandled step failure and its retry or terminal failure decision commit together.
A handled failure commits with the next checkpoint that depends on it.
Storage, encoding, and scheduling errors do not consume domain retry attempts or mark the instance failed.
They return `ErrWorkflowInfrastructure`, preserving the original error in its chain.
`CancelWorkflow`, `GetStatus`, and `GetResult` retain this classification when a step calls them.
Worker shutdown does not create a domain failure.

### Cancellation

```go
err := workflow.CancelWorkflow(ctx, runner, id, "order cancelled")
```

Cancellation is idempotent for terminal instances. It prevents later checkpoints from committing against stale state.
It cannot undo an external effect or force arbitrary user code to stop.

`WithCancellationPolicy` can enforce a maximum instance duration or delay since the last checkpoint.
The policy is checked when an instance runs. It is not an independent timer for indefinitely parked instances.

### Execution results

| Result | Meaning |
| --- | --- |
| `nil` | Completed successfully |
| `ErrWorkflowSleeping` | Paused until a persisted wake time |
| `ErrWorkflowWaiting` | Awaiting an event or timeout |
| `ErrWorkflowRetrying` | Another domain attempt is scheduled |
| `ErrWorkflowCancelled` | Persisted cancellation |
| `ErrConflictAbort` | Another execution advanced the stream |
| `ErrWorkflowInfrastructure` | Storage, encoding, configuration, or scheduling failure |
| Execution context error | Interrupted execution, without recording a domain failure |
| Other error | Domain failure, after the retry or terminal decision commits |

Use `GetResult` for completed output and `GetStatus` for persisted status and attempt number.
For backward compatibility, `GetStatus` reports sleeps and scheduled retries as `running`.
The inspector distinguishes `sleeping` and `retrying` while their deadlines are in the future.
A later failure or retry marks an elapsed sleep as completed, but leaves its exact resumption time unknown.
Cancellation does not invent sleep completion.

## Embedded monitor

`workflow/monitor` provides a read-only HTML UI and JSON API. Assets are embedded with `go:embed`.
There is no frontend build or CDN dependency.

```go
log, err := eventlog.NewSqlite(db)
if err != nil {
    return err
}
inspector, err := workflow.NewInspector(log)
if err != nil {
    return err
}
handler, err := monitor.NewHandler(inspector)
if err != nil {
    return err
}
mux.Handle("/workflows/", http.StripPrefix("/workflows", handler))
```

Open `/workflows/`. The page shows instances, filters, named steps, pauses, errors, parameters, output, and event history.
The recorded path shows executed steps, not a static graph of arbitrary Go code.
Missing legacy timestamps remain unknown. Running state does not prove a worker is alive.

**Protect the whole handler with host authentication and authorization.** Workflow data can contain secrets and personal data.
The monitor provides no mutation routes or built-in authentication.

See [monitor documentation](monitor/README.md) for API limits and security details.
`Inspector.List` requires a finite, non-tailing `event.GlobalLog` and scans history on every call.
Use an indexed read-model projection for large installations. NATS does not provide global discovery.

### Runnable example

```sh
go run ./examples/10_workflow_monitor -db /tmp/workflows.db
# Open http://127.0.0.1:8080/workflows/
```

The example starts four workers and creates completed, sleeping, waiting, failed, retrying, and cancelled instances.
On restart, use `-seed=false` to inspect and recover existing work without adding samples.

## Queue choices

| Queue | Scheduling | Recovery |
| --- | --- | --- |
| `MemoryQueue` | In-process notifications | Receipts and lease recovery within the process only |
| `SyncQueue` | SQLite task changes inside event-save transactions | Durable claims, pauses, retries, and event waits |
| `PostgresQueue` | PostgreSQL task changes inside event-save transactions | Networked workers, database-clock leases, and transactional checkpoints |
| `AsyncQueue` | Global event projection into SQLite | Durable task claims, eventually consistent discovery |

`NewRunner` defaults to memory scheduling and memory event publications, even with a persistent event log.
That combination is useful for tests and direct execution, not crash-safe orchestration.
`NewSqliteRunner` also uses this nontransactional scheduling default.

The async queue requires a running `AsyncProjectionRunner` and a durable checkpointer.
`WithTaskQueue` alone does not persist event publications or make event writes atomic with task changes.
Start that projection from a valid checkpoint before running workers.
Async notifications coalesce toward the earliest wake. Persisted sleep and retry guards prevent callbacks before their deadline.
New wait events include the workflow name. Older events use a retained identity table, not a disposable task row.
An identity-read error stops projection progress instead of creating an unrunnable task.
For older histories without a retained name, replay from the start event to rebuild identity.

Workers prefer `ClaimedTaskQueue`, which identifies each execution with a unique receipt.
A stale receipt cannot acknowledge, fail, release, or renew a newer claim.
Transactional runners also validate worker receipts during checkpoint commits. Nontransactional queues cannot provide that atomic checkpoint fence.
The original `TaskQueue` interface remains supported, but ID-only methods cannot fence overlapping claims.
The legacy memory `Poll` method remains destructive and lease-free.
Workers retry a failed legacy requeue for five seconds, including during shutdown.
If requeue fails beyond that bound, `RunWorkers` returns the retention error and instance ID.
A prolonged outage still requires a recoverable claim queue.

## Distributed failure tests

The distributed suite runs independent operating-system processes against SQLite and Postgres.
Those workers do not share Go memory, registries, connection pools, or queue objects.
The shared cases cover these contracts:

- Competing workers and unique receipts, including identical worker labels.
- An expired worker cannot commit while a replacement holds the unchanged event version.
- Forced process termination resumes from committed checkpoints without repeating earlier steps.
- Heartbeats keep long activities exclusive beyond the original lease.
- Another process publishes events or cancels an active workflow.
- Concurrent publication and wait registration do not strand indefinite waits.
- Worker replacement preserves sleep and retry deadlines.

Postgres-only storage fault tests add these cases:

- A TCP storage partition isolates one Postgres worker while a healthy worker recovers the task.
- Blackholed transaction responses do not retain task or publication locks indefinitely.
- A committed checkpoint with a lost COMMIT response replays without repeating its callback.
- A committed publication with a lost response retains its original payload and wake-up.
- A Postgres server restart preserves committed checkpoints and unfinished-task recovery.

Separate regressions cover expiry during storage waits, publication rollback, timeout races, startup concurrency, and handled-error replay.
Worker-side start and cancellation commands target other streams without inheriting the invoking checkpoint receipt.
Postgres lock tests observe real waiting connections before allowing their lease to expire.
The activity sink records visible effects. Crash tests deliberately demonstrate duplicate uncommitted effects.

With Docker available, run:

```sh
devbox run -- go test -race ./workflow -run '^TestDistributedWorkflow_' -count=3
```

These tests exercise independent processes and real TCP failures on one test machine.
They do not prove multi-region availability, Postgres failover, or every network-partition pattern.
POSIX signal tests do not run on Windows.

## Temporal replacement scope

This package supports checkpoint-based Go workflows, durable pauses, retries, recoverable workers, and a read-only monitor.
It is **not a drop-in Temporal replacement** or a Temporal protocol implementation.

Important missing capabilities include:

- Workflow-definition versioning and deterministic replay checks across incompatible deployments.
- Durable recurring signal queues and workflow queries or updates. Named publications currently use first-write-wins semantics.
- Separate activity workers, activity-specific retries and timeouts, and child-workflow lifecycle management.
- Durable heartbeat progress and automatic cancellation timers for indefinitely parked instances.
- History compaction, archival, and an indexed visibility service.
- Namespace isolation, distributed rate limits, and operator mutation controls.

Use the documented guarantees to decide whether a particular workflow can replace its current orchestration.
Keep external effects idempotent. Lease fencing protects committed state, not an effect in another system.

See [design analysis](DESIGN.md) for the recovery model and [fresh review](REVIEW.md) for additional failure paths and limits.
