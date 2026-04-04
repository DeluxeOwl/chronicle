# Workflow Package — Status & Next Steps

## What we built

A durable execution framework on top of Chronicle's event log, similar to [Temporal](https://temporal.io/) or [Absurd](https://github.com/earendil-works/absurd).

The core insight: the **event log is the state store** (workflow state, step results, sleep timestamps), and a **pluggable `TaskQueue` handles work distribution** (discovery, scheduling, claiming). These two concerns are cleanly separated, which means the same workflow code runs in single-process mode (in-memory queue) or distributed mode (persistent queue backed by projections).

### Architecture

```
Workflow author code (Step, Sleep, etc.)
         │
         ▼
    Event Log (source of truth)
    ├── workflowStarted
    ├── stepCompleted (step results, sleep timestamps)
    ├── workflowCompleted / workflowFailed
         │
         ▼
    TaskQueue (pluggable interface)
    ├── MemoryQueue        ← single-process / testing (done)
    ├── SyncProjectionQueue ← transactional event logs (TODO)
    └── AsyncProjectionQueue ← any event log (TODO)
         │
         ▼
    RunWorker() polls queue → looks up registry → calls Run() → replay from event log
```

### Public API (unchanged from original design)

```go
wf := workflow.New(runner, "send-reports", func(wctx *workflow.Context, params *P) (*O, error) {
    result, err := workflow.Step(wctx, func(ctx context.Context) (T, error) { ... })
    err := workflow.Step2(wctx, func(ctx context.Context) error { ... })
    err := workflow.Sleep(wctx, 24*time.Hour)
    return output, nil
})

instanceID, err := wf.Start(ctx, params)
output, err := wf.Run(ctx, instanceID)          // direct execution
output, err := wf.GetResult(ctx, instanceID)     // retrieve completed result
runner.RunWorker(ctx, workflow.WorkerOptions{})   // worker loop (new)
```

### Key files

| File | Purpose |
|---|---|
| [`workflow/workflow.go`](workflow.go) | Runner, Context, Workflow, New, Start, Run, Step, Step2, GetResult, GetStatus, workflow registry |
| [`workflow/workflow_sleep.go`](workflow_sleep.go) | Sleep, ErrWorkflowSleeping, RunnerOptions (WithNowFunc, WithTaskQueue) |
| [`workflow/workflow_retry.go`](workflow_retry.go) | RetryStrategy, ErrWorkflowRetrying, exponential backoff, scheduleRetry |
| [`workflow/workflow_events.go`](workflow_events.go) | AwaitEvent, EmitEvent, ErrWorkflowWaiting, ErrEventTimeout, waitStore |
| [`workflow/workflow_heartbeat.go`](workflow_heartbeat.go) | Heartbeat (ExtendLease wrapper) |
| [`workflow/queue.go`](queue.go) | `TaskQueue` interface (incl. `ExtendLease`) and `QueuedTask` type |
| [`workflow/queue_memory.go`](queue_memory.go) | In-memory TaskQueue implementation |
| [`workflow/worker.go`](worker.go) | `RunWorker()` — polling loop that drives workflows from the queue |

### Key design decisions

1. **Sleep is a step.** The `sleepResult{WakeAt, Duration}` is stored as a `stepCompleted` event. On replay, the decision is pure: `now.Before(wakeAt)` → still sleeping, else continue. Any process can make this decision.

2. **Sleep enqueues a delayed task.** Instead of in-memory timers (`time.AfterFunc`), Sleep calls `queue.Enqueue(ctx, QueuedTask{RunAfter: wakeAt})`. The queue's `Poll()` only returns tasks whose `RunAfter` has passed.

3. **`New()` registers workflows in the runner.** The runner maintains a `map[string]executableWorkflow` so that `RunWorker()` can look up the correct function by name when it polls a task from the queue.

4. **`Start()` always enqueues.** After saving the `workflowStarted` event, it enqueues a task for immediate execution. `Run()` still works for direct/manual execution (tests, simple use).

5. **`Run()` doesn't touch the queue.** It's the direct execution path. The queue is only used by `Start()` (to enqueue), `Sleep()` (to enqueue delayed), and `RunWorker()` (to poll and drive).

### What was removed

The old `Scheduler` interface, `timerScheduler`, `WaitForWake()`, `Close()`, `sleepWakes`/`wakeChs` maps, and `runnerFields` struct. All replaced by `TaskQueue`.

---

## Implemented

### ✅ Retry with backoff (`workflow_retry.go`)

Workflows can now be started with a `RetryStrategy` that configures automatic retries with exponential backoff when steps fail:

```go
instanceID, err := wf.Start(ctx, params, workflow.WithRetryStrategy(workflow.RetryStrategy{
    MaxAttempts: 10,
    BaseDelay:   1 * time.Second,
    Factor:      2.0,
    MaxDelay:    5 * time.Minute,
}))
```

**Key design decisions:**
- Retry strategy is persisted in the `workflowStarted` event, so it survives process crashes.
- Each retry records a `workflowRetried` event that bumps the attempt counter and resets status to `StatusRunning`.
- Completed steps from previous attempts are preserved (replayed, not re-executed). Failed steps re-execute.
- When max attempts are exhausted, the workflow permanently fails with `workflowFailed`.
- `ErrWorkflowRetrying` signals to the worker that a delayed retry task has been enqueued.
- `DefaultRetryStrategy()` provides sensible defaults (5 attempts, 1s base, 2x factor, 5min max).
- `GetStatus()` returns `InstanceInfo{Status, Attempt}` for monitoring.

### ✅ Heartbeat / ExtendLease (`workflow_heartbeat.go`)

The `TaskQueue` interface now includes `ExtendLease()`, and workflow authors can call `Heartbeat(wctx, duration)` from inside long-running steps:

```go
result, err := workflow.Step(wctx, func(ctx context.Context) (string, error) {
    for i := range 100 {
        processChunk(i)
        if err := workflow.Heartbeat(wctx, 5*time.Minute); err != nil {
            return "", err
        }
    }
    return "done", nil
})
```

The `MemoryQueue` no-ops on `ExtendLease`. Real lease extension will matter once a persistent queue with claiming is implemented.

### ✅ AwaitEvent / EmitEvent (`workflow_events.go`)

Workflows can now wait for external signals, following the same durable pattern as Sleep:

```go
// Inside a workflow — suspend until the event arrives
shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "order.shipped:order-42")

// With timeout
shipment, err := workflow.AwaitEvent[ShipmentPayload](wctx, "order.shipped:order-42",
    workflow.AwaitEventOptions{Timeout: 24 * time.Hour})

// From anywhere — emit an event to wake waiting workflows
err := workflow.EmitEvent(ctx, runner, "order.shipped:order-42",
    ShipmentPayload{TrackingNumber: "XYZ"})
```

**Key design decisions:**
- `AwaitEvent` records a `workflowWaiting` event + a `stepCompleted` checkpoint. On replay, if the event has arrived, the payload is returned immediately.
- `EmitEvent` is idempotent: first-write-wins. Subsequent emits for the same event name are ignored.
- The `waitStore` is an in-memory structure that tracks waiters and emitted events. For distributed systems, this would need to be backed by a persistent store (sync/async projection).
- Events can arrive before the workflow reaches `AwaitEvent` — the `Register` call checks for already-emitted events.
- Timeouts enqueue a delayed wake-up task. If the event doesn't arrive by the deadline, `ErrEventTimeout` is returned (and can be retried if a retry strategy is configured).
- Multiple workflows can wait for the same event name — all are woken when it's emitted.
- New statuses: `StatusWaiting` (waiting for event).
- New events: `workflowWaiting`, `workflowEventReceived`.

---

## Remaining steps

### 1. `queue_sync.go` — SyncProjection-backed queue (transactional event logs)

**Why:** The `MemoryQueue` is lost on process crash. For transactional event logs (SQLite, Postgres), we can atomically populate a `ready_tasks` table in the same transaction as event writes. This gives us Absurd-level guarantees.

**How:** Implement `TaskQueue` backed by a SQL table. Use Chronicle's [`SyncProjection`](../event/projection_sync.go) or [`TransactionalAggregateProcessor`](../aggregate/repository_transactional.go) to atomically insert rows when workflow events are written.

```
Event append (same tx) → SyncProjection → INSERT INTO ready_tasks
Worker → SELECT ... WHERE run_after <= NOW() AND claimed_by IS NULL (with row locking)
```

**Key concern:** Claiming. Use `UPDATE ... SET claimed_by = ?, claimed_until = ? WHERE id = ? AND claimed_by IS NULL` — one worker wins, others get 0 rows affected. Lease expiry (`claimed_until`) handles crashed workers. The `ExtendLease` method is already in the `TaskQueue` interface.

**Reference:** The [outbox pattern example](../docs/projections/example-with-outbox.md) already demonstrates atomic writes to a side table via `TransactionalAggregateProcessor` — the queue is the same pattern.

### 2. Persistent event waiting (`waiting_events` table)

**Why:** The current `waitStore` is in-memory. For distributed systems, we need a persistent store so that `EmitEvent` from one process can wake workflows running on another.

**How:** A `waiting_events` SQL table populated via SyncProjection when `workflowWaiting` events are written. `EmitEvent` queries this table instead of the in-memory store.

### 3. `queue_async.go` — AsyncProjection-backed queue (any event log)

**Why:** For non-transactional event logs (memory, or future backends like NATS/DynamoDB), we can't atomically write events + enqueue. Instead, use Chronicle's [`AsyncProjectionRunner`](../event/projection.go) to build the queue view from the global event log.

**How:** An `AsyncProjection` that watches for `workflow/started`, `workflow/step_completed` (with sleep data), `workflow/completed`, `workflow/failed`, `workflow/retried` events and populates a `ready_tasks` table accordingly.

**Trade-off:** Task discovery is eventually consistent (delayed by `pollInterval`, default 200ms). Execution is still correct — the event log's optimistic concurrency prevents duplicate step execution.

### 4. Cancellation policies

**Why:** Long-running workflows need to be cancellable — either programmatically or via time-based policies.

**How:** Like Absurd's `CancellationPolicy`:
- `MaxDuration` — cancel the task if it has been alive longer than N seconds
- `MaxDelay` — cancel the task if no checkpoint has been written for N seconds
- New event: `workflowCancelled`
- `CancelWorkflow(ctx, runner, instanceID)` API

---

## Priority order

1. **`queue_sync.go`** — this is the path to production-grade distributed workers
2. **Persistent event waiting** — needed for distributed AwaitEvent/EmitEvent
3. **`queue_async.go`** — only needed for non-transactional backends, lower priority
4. **Cancellation policies** — nice-to-have for production robustness
