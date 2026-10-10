# Workflow analysis and implementation decisions

## Request

Extend the existing `workflow/` package from first principles on the current branch.
Add the missing worker capabilities and an embedded HTML view of workflows.

**Additional acceptance criterion:** test distributed execution in independent processes, including competing workers, process termination, lease recovery, and cross-process event publication.
The intended direction is a Temporal replacement. That goal requires failure-mode tests, not only successful in-process execution.
This implementation must document which guarantees it provides and which Temporal capabilities remain absent.

## First-principles model

A durable workflow needs three separate responsibilities:

1. **History:** the event stream records committed checkpoints and scheduling intent.
2. **Scheduling:** a recoverable queue says which instance needs attention and when.
3. **Execution:** workers claim tasks, replay checkpoints, and run unfinished code.

Inspection is a fourth, read-only responsibility. It must not execute workflow definitions.

The event stream is authoritative. Queue tasks are wake-up notifications, not proof of workflow completion.
A claim receipt identifies one execution. An instance ID or worker ID cannot identify overlapping claims.

```text
                     workflow event stream
                             |
             +---------------+----------------+
             |                                |
     transactional task changes          read-only Inspector
             |                                |
     recoverable claim queue             embedded HTTP monitor
             |
       bounded worker pool
             |
     replay completed checkpoints
             |
       run unfinished step
             |
    commit checkpoint or pause intent
```

## Audit findings and changes

| Initial gap | Consequence | Implemented correction |
| --- | --- | --- |
| One worker loop, no bounded pool | Applications must duplicate lifecycle code | `RunWorkers` bounds execution and joins workers and heartbeats |
| Claims identified by worker ID | An old execution can modify a newer claim | Receipt-aware queue operations, including the memory worker path |
| Event versions alone fence checkpoints | An expired worker can save before its replacement writes an event | Validate receipts inside the event-save transaction |
| Lease time sampled before storage waits | A delayed operation can renew expired ownership | Acquire the connection and write lock before the clock read |
| SQLite-only transactional scheduling | Workers cannot share a networked database across hosts | `PostgresQueue` with database-clock leases and transactional waits |
| Half-open transaction retains storage locks | Lease expiry cannot recover a row held by an idle transaction | Bounded client execution and server timeouts installed before locks |
| Enqueue can replace a live claim | Two workers can run the same instance before lease expiry | Preserve ownership and store a pending successor |
| Infrastructure errors treated like execution failure | Unfinished work can disappear or become terminal | Typed infrastructure errors and recoverable task release |
| Publication errors inside steps lack infrastructure classification | A storage failure consumes domain retries or terminates a workflow | Classify persistence and wake-up errors with the infrastructure sentinel |
| Lagging async wait projection replaces newer wakes | A published event waits until an obsolete deadline | Coalesce async notifications toward the earliest wake |
| Timeout decision uses an unlocked publication read | An on-time event can lose to a stale timeout decision | Validate timed-out checkpoints under the publication lock |
| Step failure saved before retry decision | A crash can strand or prematurely rerun the instance | Commit failure and retry or terminal decision together |
| Direct runs ignore retry scheduling | Callers can bypass backoff | Readiness guards use persisted sleep and retry deadlines |
| Context shutdown follows domain failure handling | Stopping workers can fail otherwise valid work | Cancellation stops execution without a domain failure event |
| Published event and wake-up commit separately | A crash can leave an indefinite wait stranded | Transactional publication and wake-up in the SQLite store |
| Timeout checked before publication | A late worker discards an on-time event | Persist and compare publication time with the wait deadline |
| Pre-published events require another run | Memory workers can park forever | Resolve available publications during the first wait call |
| Retry limit zero contradicts documentation | Zero creates unlimited retries | Normalize zero to five total attempts |
| No query or UI surface | Operators cannot inspect committed progress | Inspector, embedded monitor, and runnable SQLite example |
| No start metadata for active steps | Operators cannot identify the active callback | Optional named-step start events and recorded timestamps |
| Sleep inferred from arbitrary result JSON | A user result can schedule a false pause | Persist explicit step kind, retaining legacy decoding only for old events |

## Safety and liveness

### Safety

- Optimistic event-stream concurrency prevents conflicting checkpoints from both committing.
- Receipts fence queue operations against stale executions, including executions in the same process.
- Transactional queue processors validate worker ownership before the event-save transaction commits. Event versions alone are insufficient.
- SQLite samples lease time after write-lock acquisition. Postgres uses fresh database time after the task-row lock.
- Sleep, retry, and event publication preserve their durable scheduling intent.
- A timed-out wait checkpoint rechecks on-time publication under the same storage lock before its event append commits.
- `NewSqliteRunnerWithSyncQueue` and `NewPostgresRunnerWithSyncQueue` commit task changes with event writes. Publication also commits its wake-up tasks atomically.
- Postgres wait operations use a shared advisory lock and read-committed transactions. Wait registration and publication cannot both overlook each other.
- Read-only inspection neither registers nor calls workflow functions.

### Liveness

A crashed execution becomes eligible after its lease expires.
Postgres storage transactions use client deadlines and transaction-local statement and idle-transaction timeouts.
These limits prevent a blackholed response from retaining workflow locks indefinitely. The default limit is five seconds.
The transaction limit does not apply to callback execution.

A cancelled worker releases unfinished work with bounded cleanup where the queue supports release.
Transient failures retain tasks. Unknown workflow names remain recoverable after their definitions are registered.
Workers claim work only when an execution slot is available.

These guarantees require available storage, working queue discovery, and callbacks that honor cancellation.
Workflow deadlines and SQLite leases require synchronized clocks. Postgres leases use the database clock.
An async scheduler also requires its projection and checkpointer.

## Compatibility decisions

The existing `RunWorker` and `TaskQueue` APIs remain available.
Workers use the stronger receipt interface when supported.
Legacy ID-only operations remain unsuitable for overlapping claims.

Named starts are optional. Existing unnamed workflows keep completion-only tracing.
Additive event fields keep older JSON readable. Legacy timestamps are not invented.
Older step-failure events are nonterminal unless a terminal workflow event follows.

The inspector derives deferred display statuses from deadlines.
The aggregate status API retains its existing `running` value for sleeps and scheduled retries.

## Why the monitor shows a recorded path

Workflow definitions are arbitrary Go functions. Their branches can depend on checkpoint results.
There is no truthful static DAG without a separate declarative definition model.
The monitor therefore shows recorded steps in positional order and the full event history.
It can show retries and pauses without claiming to know future branches.

The monitor is a standard-library HTTP handler with embedded assets.
It can mount under a host-selected path and requires no separate service or frontend build.
Host middleware owns authentication, authorization, timeouts, and rate limits.

## Distributed acceptance tests

The earlier suite simulated most restarts within one process.
The added suite re-executes the test binary in independent processes with separate database handles.
SQLite provides same-host storage. Postgres provides a shared TCP database for workers on separate hosts.

| Failure scenario | Observable contract |
| --- | --- |
| Competing processes with identical worker labels | Unique claim receipts and no duplicate active execution |
| Lease expires before a stalled callback returns | Its checkpoint rolls back, even without an event-version change |
| Process receives `SIGKILL` after one checkpoint | A replacement replays the saved result and repeats only unfinished work |
| Heartbeat owner runs beyond its initial lease | A competing worker does not execute the same activity |
| Postgres worker clock differs by one hour | Database time, not the worker clock, controls its lease |
| Publisher exits while the workflow worker is absent | A later worker consumes the durable publication |
| Publication races wait registration | An indefinite wait does not lose its wake-up |
| Another process cancels an active workflow | The callback cannot commit over the terminal cancellation |
| Storage sockets drop for one worker | A healthy worker recovers the task without consuming domain retries |
| Transaction responses disappear while sockets stay open | Server timeouts release task and publication locks |
| Worker stops during a sleep or retry delay | A replacement preserves the recorded deadline |
| SQLite connection or write lock crosses expiry | A delayed receipt operation cannot revive expired ownership |
| Postgres receipt operation waits beyond expiry | Fresh database time rejects it after the row lock |
| Publication wake-up write fails | The payload and wake-up roll back together |
| On-time publication races a timeout checkpoint | The timeout rolls back without consuming a domain retry |

The external activity sink records visible effects independently of Chronicle history.
Forced-crash tests expect duplicate effects for an unfinished callback. They reject duplicate effects for earlier committed steps.

Run the process suite with Docker available:

```sh
devbox run -- go test -race ./workflow -run '^TestDistributedWorkflow_' -count=3
```

The suite runs locally, with real independent processes and a TCP fault proxy.
It does not establish multi-region availability or Postgres replication and failover guarantees.
POSIX termination tests do not run on Windows.

## Verification

The following checks passed during implementation:

```sh
devbox run -- task test
devbox run -- task lint
devbox run -- go test -race ./workflow/... -count=3
```

The embedded monitor also passed browser checks for nested paths, filters, details, empty states, refresh preservation, and mobile layout.

A mutation test disabled the Postgres idle-transaction timeout without changing client deadlines.
Both half-open recovery cases then failed. Restoring the server timeout made both cases pass.
This test distinguishes server lock recovery from client socket cancellation.

## Temporal replacement direction

The requested goal is a Temporal replacement. The current implementation provides a smaller checkpoint-based orchestration model.
It does not implement Temporal APIs, protocols, or all operational guarantees.

Before replacing a Temporal deployment, compare each workflow against these missing capabilities:

- Workflow-definition versions and deterministic replay validation across deployments.
- Durable recurring signals, workflow queries, updates, and child-workflow lifecycle management.
- Separate activity queues, per-activity retry policies, activity timeouts, and durable heartbeat progress.
- An independent cancellation scheduler for indefinitely parked workflows.
- Indexed visibility, history compaction, and archival.
- Namespace isolation, distributed rate limits, and operator mutation controls.

Independent process tests establish specific recovery contracts. They do not establish feature parity.
External effects remain at least once, including when stale workers cannot commit checkpoints.

## Boundaries and remaining limits

- External effects are **at least once**. Receipts fence queue changes, not effects in other systems.
- Positional replay requires compatible workflow definitions across deployments. Names detect some changes but do not provide definition versioning.
- Registry mutation is synchronized, but replacing a definition still requires an application deployment strategy.
- Cancellation cannot undo effects or preempt callbacks that ignore their context.
- Automatic cancellation policies run at execution boundaries, not through an independent timer for parked instances.
- `NewRunner` with memory scheduling does not recover tasks or event publications after a process restart.
- The async queue is eventually consistent. It does not make arbitrary event-log and queue pairs atomic.
- Inspection requires intact JSON workflow history. No workflow snapshot or archival integration is added.
- Listing scans the global log. Large deployments need an indexed projection.
- NATS has no global discovery surface for this monitor.
- The UI is read-only. Manual retries, cancel controls, worker telemetry, static graph definitions, and distributed rate limits are outside this implementation.

The standalone `queue/` package remains independent.
Workflow scheduling retains per-instance SQL queues because they can share transactions with workflow event writes.
