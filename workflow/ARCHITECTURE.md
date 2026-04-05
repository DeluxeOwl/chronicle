# Workflow Package Architecture

The `workflow` package builds a **durable workflow engine** on top of Chronicle's event sourcing primitives. A workflow instance is just an aggregate (`WorkflowInstance`) whose entire execution history — start, step completions, sleeps, retries, event waits, cancellations — is captured as domain events in the event log. This gives you replay, crash recovery, and auditability for free.

## Core Idea

A workflow is a plain Go function `func(*Context, *Params) (*Output, error)` composed of **steps**. Each step's result is persisted as an event. When a workflow is re-executed (after a crash, sleep, or retry), it replays from the event log: completed steps return their cached result without re-running the side-effecting function. Only the first un-cached step actually executes.

This is the same replay model as Temporal/Durable Functions, but implemented purely as event-sourced aggregates with no external orchestrator.

## Key Interfaces and Types

### `WorkflowInstance` (the aggregate)

Defined in `workflow.go`. This is an `aggregate.Aggregate` whose state is rebuilt by replaying `WorkflowEvent`s. Its fields track:

- `status` — one of `running`, `waiting`, `completed`, `failed`, `cancelled`
- `stepResults` — a `map[int][]byte` mapping step index → serialized result (the replay cache)
- `currentStep` — the next step to execute
- `attempt` — 1-indexed retry counter
- `retryStrategy`, `cancellationPolicy` — optional configs persisted in the started event

The `Apply` method is the event handler that rebuilds this state from the event stream. All workflow events are unexported types — external code interacts through `Workflow`, `Step`, `Sleep`, etc.

### `WorkflowEvent` (the sum type)

Nine event types, all in `workflow.go`, `workflow_retry.go`, `workflow_events.go`, `workflow_cancel.go`:

| Event                   | When                                                                                      |
| ----------------------- | ----------------------------------------------------------------------------------------- |
| `workflowStarted`       | `Start()` — records params, retry strategy, cancellation policy                           |
| `stepCompleted`         | `Step()`/`Step2()`/`Sleep()`/`WaitForEvent()` — stores the step index + serialized result |
| `stepFailed`            | A step function returns an error                                                          |
| `workflowCompleted`     | The workflow function returns successfully                                                |
| `workflowFailed`        | The workflow function fails with no retries left                                          |
| `workflowRetried`       | A retry is scheduled after failure                                                        |
| `workflowWaiting`       | `WaitForEvent()` — records the event name being waited for                                |
| `workflowEventReceived` | `PublishEvent()` wakes a waiting workflow                                                 |
| `workflowCancelled`     | Explicit or policy-based cancellation                                                     |

### `Workflow[Params, Output]` (the generic handle)

Created via `New(runner, name, fn)` — this registers the workflow function in the runner's internal `map[string]executableWorkflow` registry. Exposes:

- **`Start(ctx, params, ...opts)`** — creates the aggregate, records `workflowStarted`, saves to repo, enqueues a task. Returns the `InstanceID`.
- **`Run(ctx, instanceID)`** — loads the aggregate from the repo, calls the workflow function. Steps replay from cached results; the first un-cached step actually executes. Handles sleep/wait/retry/cancel sentinel errors.
- **`GetResult(ctx, instanceID)`** / **`GetStatus(ctx, instanceID)`** — read-only queries against the aggregate.

`Workflow` also implements `executableWorkflow` (an unexported interface with `execute(ctx, instanceID) error`), which is what the worker loop calls without knowing the concrete type parameters.

### `Step[Result]` and `Step2` (the step functions)

Top-level generic functions in `workflow.go`. The pattern:

1. Claim a step index from `wctx.stepCount` (a local counter incremented per Step/Sleep/WaitForEvent call)
2. Reload the aggregate from the repo
3. If `stepResults[stepIndex]` exists → deserialize and return (replay)
4. Otherwise → execute `fn(ctx)`, record `stepCompleted` (or `stepFailed`), save

**Consistency**: each step does a full reload + save cycle. The event log's optimistic concurrency prevents two workers from advancing the same step — the second writer gets a conflict error.

### `TaskQueue` (the scheduling interface)

Defined in `queue.go`. This is the abstraction that decouples "when should a workflow run" from "how it runs." Four operations:

- `Enqueue(task)` — add work, optionally with `RunAfter` for delayed execution
- `Poll(ctx)` — atomic claim-and-return of the next ready task
- `Complete(instanceID)` / `Fail(instanceID)` — cleanup after execution
- `ExtendLease(instanceID, duration)` — for long-running steps (see `Heartbeat`)

Three implementations exist, each with different consistency guarantees:

**`MemoryQueue`** (`queue_memory.go`) — mutex-protected slice. Single-process only. Tasks lost on crash. Good for testing.

**`SyncQueue`** (`queue_sync.go`) — SQL-backed (`workflow_ready_tasks` table). Implements `aggregate.TransactionalAggregateProcessor`, meaning task creation happens **inside the same SQL transaction** as the event append. If the process crashes after saving events, the task is already in the database. This is the production-grade option. Uses lease-based claiming (`claimed_by` + `claimed_until_ns`) with row-level locking for multi-worker support.

**`AsyncQueue`** (`queue_async.go`) — SQL-backed, but populated by an `event.AsyncProjection` that reads the global event stream. Eventually consistent: there's a delay between event write and task discovery. The event log's optimistic concurrency still prevents duplicate execution, so correctness is preserved — only liveness is affected. Useful for non-transactional event stores.

### `Runner` (the orchestrator)

Created via `NewRunner(eventLog, logger, ...opts)` or convenience constructors `NewSqliteRunner` / `NewSqliteRunnerWithSyncQueue`. Owns:

- The `aggregate.Repository` for `WorkflowInstance`
- The `TaskQueue`
- The `eventWaitStore` (for `WaitForEvent`/`PublishEvent`)
- The workflow registry (`map[string]executableWorkflow`)
- A `nowFunc` (injectable clock for testing)

### `eventWaitStore` (the event-wait abstraction)

Defined in `workflow_events.go`. Two implementations:

**`waitStore`** (in-memory) — maps of `eventName → []waitingWorkflow` and `eventName → payload`. First-write-wins semantics for published events. Used with `MemoryQueue`.

**`persistentWaitStore`** (`wait_store_persistent.go`) — SQL-backed using `workflow_waiting_events` and `workflow_published_events` tables. Enables distributed WaitForEvent/PublishEvent: an event published from one process wakes workflows on another. The transactional inserts/deletes of waiters are handled by `SyncQueue.Process()` inside the event-save transaction; `persistentWaitStore` handles the non-transactional reads and the `Publish` path.

## How Suspension Works

Sleep, WaitForEvent, and Retry all follow the same pattern:

1. Record the suspension as a `stepCompleted` event (with metadata like wake-at time, event name, etc.)
2. Save the aggregate
3. Enqueue a delayed task (or no task, for indefinite waits)
4. Return a sentinel error (`ErrWorkflowSleeping`, `ErrWorkflowWaiting`, `ErrWorkflowRetrying`)
5. The worker loop recognizes the sentinel, marks the current task as complete, and moves on
6. When the delayed task fires (or `PublishEvent` enqueues a wake-up), `Run` is called again — it replays all steps, sees the suspension step's cached result, checks if the condition is met (time elapsed / event arrived), and either continues or re-suspends

## Consistency Model

**Event log is the source of truth.** The task queue is a derived, disposable structure. If the queue is lost, you can rebuild it by replaying events.

**Optimistic concurrency on the aggregate** prevents two workers from concurrently advancing the same workflow instance. If worker A and worker B both poll the same task (e.g., due to lease expiry), one will fail with a version conflict when trying to save, and the task will naturally retry.

**Step idempotency via replay** — each step checks `stepResults[stepIndex]` before executing. Even if a step function has side effects (HTTP calls, etc.), replay skips it entirely and returns the cached result.

**SyncQueue transactional atomicity** — with `NewSqliteRunnerWithSyncQueue`, the `SyncQueue.Process()` method runs inside the same SQL transaction as the event append. This means task creation can never be lost separately from events. The `Process` method inspects the committed events and maps them to task operations (e.g., `workflowStarted` → upsert immediate task, `stepCompleted` with sleep data → upsert delayed task, `workflowCompleted` → delete task).

## Recommended Reading Order

If you want to read through the code to build understanding, this order mirrors how the concepts layer on each other:

1. **`queue.go`** — the `TaskQueue` interface and `QueuedTask` struct. Small file, sets the vocabulary.
2. **`workflow.go`** — the heart. Read `WorkflowInstance` (aggregate + events + Apply), then `Runner`, then `Workflow[P,O]` (Start, Run), then `Step`/`Step2`. This one file gives you 80% of the mental model.
3. **`worker.go`** — the poll loop. Short file that ties `TaskQueue` + `executableWorkflow` together.
4. **`workflow_sleep.go`** — first suspension primitive. Shows the record-enqueue-sentinel pattern.
5. **`workflow_events.go`** — `WaitForEvent`/`PublishEvent` and the `eventWaitStore` interface + in-memory impl.
6. **`workflow_retry.go`** — `RetryStrategy` and `scheduleRetry`. Same suspension pattern as sleep.
7. **`workflow_cancel.go`** — `CancellationPolicy` and `CancelWorkflow`.
8. **`workflow_heartbeat.go`** — one-liner that calls `ExtendLease`. Shows how long-running steps stay alive.
9. **`queue_memory.go`** — trivial `TaskQueue` impl. Read it to ground the interface.
10. **`queue_sync.go`** — the production `TaskQueue`. Focus on `Process()` to see how events map to task operations, and `Poll()` for lease-based claiming.
11. **`queue_async.go`** — async projection variant. Compare with `SyncQueue` to see the consistency trade-off.
12. **`wait_store_persistent.go`** — SQL-backed event waiting for distributed setups.

---

## Q&A

### 1. What is missing compared to Temporal?

The big ones:

- **Signals** — Temporal signals are fire-and-forget messages delivered to a running workflow that can be handled at any point, buffered, and processed in order. `WaitForEvent`/`PublishEvent` is a subset: it's a single-shot "wait for one named event" pattern. You can't receive multiple signals of the same type, you can't buffer them, and you can't handle them at arbitrary points in the workflow function. To match Temporal you'd need a durable signal inbox on the aggregate (append signals as events, drain them inside the workflow function).

- **Queries** — Temporal lets external callers synchronously query a workflow's in-memory state without mutating it. There's no equivalent here. `GetStatus`/`GetResult` exist but they just load the aggregate from the event log — they can't run arbitrary query handlers against the live workflow state. Not hard to add (you already have the aggregate), but the API doesn't expose it.

- **Updates** — Temporal's newer update primitive (synchronous signal + response). Completely absent.

- **Child workflows** — Temporal can spawn sub-workflows, wait for their results, and cancel them as a tree. This package has no notion of parent/child. You could start another workflow from inside a `Step`, but there's no built-in coordination, cancellation propagation, or result forwarding.

- **Continue-as-new** — Temporal can close a workflow and start a fresh one with new params to avoid unbounded event history growth. Since each `WorkflowInstance` is an aggregate, its event stream grows forever. With enough steps you'll hit replay performance issues. Snapshots from the main Chronicle framework could help, but there's no `ContinueAsNew` primitive.

- **Activity task queues / worker routing** — Temporal can route activities to specific worker pools (e.g., GPU workers vs IO workers). Here the worker loop is a single `RunWorker` with one poll loop. There's no task-queue-per-activity or worker affinity.

- **Cron / scheduled workflows** — Temporal has native cron schedules. Not present here.

- **Versioning / patching** — Temporal has `workflow.GetVersion()` for safely changing workflow code while old executions are still running. Here if you change the workflow function, replaying old instances will diverge (different step count, different types). There's no migration story.

- **Search attributes / visibility** — Temporal indexes custom attributes for listing/filtering workflows. No equivalent here — you'd need to build a projection.

- **Multi-tenancy / namespaces** — Not present.

- **Saga / compensation** — Not built-in. You could implement it manually by catching errors and running compensating steps, but there's no framework support.

### 2. Does AsyncQueue really work with any event log? Can I mix SQS queue + Postgres event log, or SQLite queue + Postgres event log?

The `AsyncQueue` implements `event.AsyncProjection`, which means it consumes `GlobalRecord`s from a `GlobalReader`. So the constraint is: **the event log must implement `event.GlobalLog`** (i.e., have a global ordered stream). If your event log only implements `event.Log` (per-aggregate reads), the async projection runner can't drive the `AsyncQueue`.

Assuming the event log has a global stream, the mixing works like this:

- **Postgres event log + SQLite async queue**: Works. The `AsyncProjectionRunner` reads from Postgres's global stream, calls `AsyncQueue.Handle()`, which writes to a local SQLite `workflow_ready_tasks` table. The worker polls that SQLite table. Eventual consistency: there's a lag between event commit in Postgres and task appearing in SQLite (the projection's poll interval). Correctness is preserved because the event log's optimistic concurrency prevents two workers from saving the same step — one will get a version conflict.

- **Postgres event log + SQS queue**: You'd need to write a `TaskQueue` implementation backed by SQS. The `TaskQueue` interface is simple (Enqueue/Poll/Complete/Fail/ExtendLease), so this is doable. But you'd lose the projection-driven task creation — you'd need either a custom `AsyncProjection` that publishes to SQS, or you'd rely on the `Enqueue` calls that `Start`/`Sleep`/`WaitForEvent` make directly. The direct `Enqueue` calls happen *outside* the event-save transaction though, so you'd have the classic dual-write problem: event saved, process crashes before SQS publish → lost task. You'd need something to reconcile (e.g., a projection that scans for "started but no matching SQS message").

- **SQLite event log + SQLite async queue** (same DB): This is a downgrade from `SyncQueue`. The `AsyncQueue` uses separate, non-transactional SQL statements, so even though they hit the same DB file, task creation is NOT atomic with event writes. You'd get the same eventual-consistency gap. Use `SyncQueue` with `NewSqliteRunnerWithSyncQueue` if both are the same SQLite DB — that's the whole point of the transactional setup.

The general rule: **`SyncQueue` gives you transactional atomicity (events + tasks in one commit) but requires the queue and event log to share the same SQL database and transaction. `AsyncQueue` decouples them but introduces eventual consistency.** Correctness (no duplicate step execution) is always preserved by optimistic concurrency on the aggregate. What you lose with async is liveness: a task might be delayed or, in edge cases, lost until a recovery mechanism re-creates it.

### 3. Is the WaitForEvent/PublishEvent state implemented in a transactionally safe way? Does it matter?

It depends on which setup you're using.

**With `MemoryQueue` + in-memory `waitStore`** (the default from `NewRunner`): No transactional safety at all. The sequence in `WaitForEvent` is:

1. `repo.Save()` — persists `workflowWaiting` + `stepCompleted` events
2. `waitStore.Register()` — writes to in-memory maps
3. `queue.Enqueue()` — writes to in-memory slice (for timeout task)

If the process crashes between step 1 and step 2, the events are saved but no waiter is registered. When the workflow is re-run, it will replay, see the step is cached but unresolved, and... call `waitStore.GetEvent()` which returns false (no registration, no published event). It will return `ErrWorkflowWaiting` — but nobody is listening for the event anymore. The workflow is stuck unless you re-register waiters on startup by scanning the event log. This is a known gap for the in-memory path, but it's the testing/single-process path so it's acceptable.

Also, if `PublishEvent` is called *before* `WaitForEvent` (the pre-publish case), the in-memory `Register()` method detects this by checking `ws.published[eventName]` and stores the payload for the instance. On next `Run`, `GetEvent` finds it. This works within a single process lifetime but is lost on restart.

**With `SyncQueue` + `persistentWaitStore`** (from `NewSqliteRunnerWithSyncQueue`): This is the safe path. The `SyncQueue.Process()` method runs inside the same SQL transaction as the event append. When it sees a `workflowWaiting` event, it atomically:

1. Inserts into `workflow_waiting_events` table
2. Checks `workflow_published_events` for pre-published events
3. If pre-published: upserts an immediate task into `workflow_ready_tasks`
4. If not pre-published but has deadline: upserts a delayed task

All of this happens in the same `*sql.Tx` as the event write. So there's no window where the event is saved but the waiter isn't registered.

The `PublishEvent` path (`persistentWaitStore.Publish`) runs its own transaction: INSERT the published event, SELECT matching waiters, DELETE them. Then it calls `queue.Enqueue()` for each waiter. The `Enqueue` is a separate SQL statement (not in the Publish transaction). So there *is* a small gap: if the process crashes after `Publish`'s transaction commits but before all `Enqueue` calls complete, some waiters won't get a wake-up task. However, the published event is already persisted, so on the next `Run` of those workflows (e.g., triggered by a timeout task or manual re-run), `GetEvent` will find the published payload and resolve the wait. The data is safe; only the immediate wake-up might be delayed.

**Does it matter?** For correctness: no. The event log is always the source of truth. A "lost" task or "lost" waiter registration is a liveness issue, not a safety issue — the workflow won't execute a step twice or produce wrong results. For liveness: yes, with the in-memory setup you can get stuck workflows after a crash. With the `SyncQueue` setup, the worst case is a delayed wake-up (event timeout or manual re-trigger resolves it). If you care about production reliability, use `NewSqliteRunnerWithSyncQueue`.

### 4. Does the workflow use optimistic concurrency? Is it correct?

**Yes, optimistic concurrency is always active** — it's baked into the framework. Every `repo.Save()` call goes through `aggregate.CommitEvents` (or `CommitEventsWithTX` for the transactional repo), which computes:

```go
expectedVersion := version.CheckExact(
    root.Version() - version.Version(len(uncommittedEvents)),
)
```

...and passes it to `AppendEvents`. If another writer appended events in the meantime, the store returns a `version.ConflictError`. This applies to every Save in the workflow package — `Start`, `Step`, `Step2`, `Sleep`, `WaitForEvent`, `scheduleRetry`, `CancelWorkflow`, and the final completion save in `Run`.

So two workers cannot both successfully save the same step. One will win, the other gets a `ConflictError`. **The data is safe.**

**However, the workflow code doesn't handle `ConflictError` gracefully.** This is the actual gap. Look at `Step`:

```go
if _, _, err := wctx.runner.repo.Save(wctx.ctx, instance); err != nil {
    return zero, fmt.Errorf("save step result: %w", err)
}
```

The `ConflictError` propagates up through the workflow function as a regular error. In `Run`, it hits this path:

```go
output, err := w.fn(wctx, &params)
if err != nil {
    // checks for sleep, waiting, timeout sentinels...
    // then:
    retried, retryErr := w.runner.scheduleRetry(wctx, instance, err)
    // if no retry configured:
    instance.recordThat(&workflowFailed{Error: err.Error()})
}
```

So a `ConflictError` — which is a harmless "someone else already did this" — gets treated as a workflow failure. If the workflow has a `RetryStrategy`, it wastes a retry attempt. If it doesn't, the workflow is marked as permanently **failed** even though the other worker successfully advanced it.

The worker loop makes this slightly less catastrophic because it marks the task as failed and moves on. The other worker (the one that won the race) will have already made progress, and its task will complete normally. But the `workflowFailed` event is now in the event log for an instance that's actually fine — that's incorrect state.

Wait — actually it's worse. `scheduleRetry` reloads the instance first:

```go
instance, loadErr := w.runner.repo.Get(ctx, instanceID)
retried, retryErr := w.runner.scheduleRetry(wctx, instance, err)
```

So it loads the latest version (which includes the other worker's step), then tries to record `workflowRetried` or `workflowFailed` on it and save. That save *might* succeed (if no one else is writing), which would incorrectly mark a progressing workflow as failed/retrying.

**Why doesn't this blow up in practice?**

1. **Concurrent execution is unlikely.** With `MemoryQueue`, `Poll` removes the task — only one worker gets it. With `SyncQueue`, lease-based claiming means a second worker only picks up the task if the first worker's lease expires (30s default). So the window for races is small.

2. **But it's not impossible.** A slow step (e.g., HTTP call that takes 35s) can cause lease expiry, and then you have two workers on the same instance. `Heartbeat` exists to extend leases, but the workflow author must call it manually.

**What should be done to fix it:**

The workflow code should catch `version.ConflictError` at the `Step`/`Step2`/`Sleep`/`WaitForEvent` level and treat it as "someone else made progress, abort this execution silently" rather than letting it propagate as a workflow failure. Something like:

```go
if _, _, err := wctx.runner.repo.Save(wctx.ctx, instance); err != nil {
    var conflictErr *version.ConflictError
    if errors.As(err, &conflictErr) {
        return zero, ErrConflictAbort // new sentinel, handled like ErrWorkflowSleeping in the worker
    }
    return zero, fmt.Errorf("save step result: %w", err)
}
```

Alternatively, the runner could wrap the repo with `chronicle.NewEventSourcedRepositoryWithRetry` (the existing `ESRepoWithRetry` decorator from `aggregate/repository_conflict_retry.go`), which retries saves on `ConflictError`. But that only helps if the retry re-loads the aggregate first — the current `ESRepoWithRetry.Save` retries the same save with the same stale version, which would just conflict again. So the retry wrapper alone isn't sufficient; the Step function would need to re-load and re-check the cache before re-saving.

**TL;DR: Optimistic concurrency is active and prevents data corruption. But `ConflictError` is mishandled — it's treated as a workflow failure instead of being recognized as a benign race condition. The fix is straightforward but not implemented yet.**

### 5. Is there any way to query/list workflows? (Dashboard use case)

**No.** The workflow package has no listing, filtering, or search capability.

The only read APIs are point lookups that require you to already have the `InstanceID`:

- `GetResult(ctx, instanceID)` — returns the output of a completed instance
- `GetStatus(ctx, instanceID)` — returns `InstanceInfo{Status, Attempt}`

There is no way to:
- List all workflow instances
- Filter by workflow name (e.g., "show me all `orders` workflows")
- Filter by status, time range, or custom metadata/labels
- Get step-level progress or history for a given instance
- Power a dashboard or admin UI

The underlying event log doesn't help either. `ReadEvents(logID, ...)` reads a single aggregate's stream (you need the ID). `ReadAllEvents(...)` reads the global stream by version — useful for projections, but not for ad-hoc queries.

The `workflow_ready_tasks` table has `instance_id` and `workflow_name`, but it only contains *active* tasks — completed and failed workflows are deleted from it. So it can't serve as a query source.

**The `WorkflowInstance` aggregate also doesn't carry user-defined metadata.** There's no labels/tags/attributes field on `workflowStarted` or anywhere else. Even if you could list instances, you couldn't filter by business context (e.g., "all workflows for tenant X" or "all workflows for order-42").

**How you'd add it:** Build a projection. This is the standard event-sourcing answer — the workflow events are already in the event log, so you write an `AsyncProjection` (or a `TransactionalAggregateProcessor` for strong consistency) that watches workflow events and maintains a read-model table:

```sql
CREATE TABLE workflow_instances_view (
    instance_id TEXT PRIMARY KEY,
    workflow_name TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at INTEGER,
    completed_at INTEGER,
    current_step INTEGER,
    attempt INTEGER,
    params TEXT,
    output TEXT,
    error_message TEXT
    -- custom metadata columns as needed
);

CREATE INDEX idx_workflow_view_name ON workflow_instances_view (workflow_name);
CREATE INDEX idx_workflow_view_status ON workflow_instances_view (status);
```

The projection would handle each event type:
- `workflow/started` → INSERT row with status=running, params, started_at
- `workflow/step_completed` → UPDATE current_step
- `workflow/completed` → UPDATE status=completed, output, completed_at
- `workflow/failed` → UPDATE status=failed, error_message
- `workflow/retried` → UPDATE attempt, status=running
- `workflow/cancelled` → UPDATE status=cancelled

For custom metadata (tenant, order ID, etc.), you'd need to either:
1. Extend `workflowStarted` to carry a `metadata map[string]string` field (requires changing the workflow package)
2. Extract metadata from the serialized `params` in the projection (fragile, couples the projection to every workflow's param type)
3. Add a separate `workflowLabelled` event type

None of this is shipped. Compare with Temporal, which has built-in search attributes, a visibility store, and `ListWorkflowExecutions` with SQL-like filtering out of the box.
