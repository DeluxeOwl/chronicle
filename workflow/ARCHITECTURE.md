# Workflow Package — Architecture Overview

The `workflow` package is a **durable workflow engine** built on top of Chronicle's event sourcing primitives. It provides Temporal/Inngest-style workflow orchestration where every step, sleep, retry, and event wait is durably persisted as events in the event log, enabling crash-resilient, resumable workflows.

---

## Table of Contents

- [Workflow Package — Architecture Overview](#workflow-package--architecture-overview)
  - [Table of Contents](#table-of-contents)
  - [High-Level Architecture](#high-level-architecture)
  - [Core Concepts](#core-concepts)
    - [WorkflowInstance (Aggregate)](#workflowinstance-aggregate)
    - [Runner](#runner)
    - [Workflow\[Params, Output\]](#workflowparams-output)
    - [Worker Loop](#worker-loop)
    - [TaskQueue](#taskqueue)
    - [Steps](#steps)
  - [Event Model](#event-model)
  - [Execution Flow](#execution-flow)
    - [Starting a Workflow](#starting-a-workflow)
    - [Executing Steps](#executing-steps)
    - [Replay \& Resume](#replay--resume)
  - [Durable Primitives](#durable-primitives)
    - [Sleep](#sleep)
    - [Retry with Backoff](#retry-with-backoff)
    - [AwaitEvent / EmitEvent](#awaitevent--emitevent)
    - [Heartbeat](#heartbeat)
    - [Cancellation](#cancellation)
  - [TaskQueue Implementations](#taskqueue-implementations)
    - [MemoryQueue](#memoryqueue)
    - [SyncQueue (SQLite, Transactional)](#syncqueue-sqlite-transactional)
    - [AsyncQueue (Eventually Consistent)](#asyncqueue-eventually-consistent)
  - [Event Wait Store](#event-wait-store)
  - [Component Diagram](#component-diagram)
  - [Data Flow Summary](#data-flow-summary)
    - [Happy Path (Step Execution)](#happy-path-step-execution)
    - [Sleep](#sleep-1)
    - [Retry](#retry)
    - [AwaitEvent / EmitEvent](#awaitevent--emitevent-1)

---

## High-Level Architecture

```
┌─────────────────────────────────────────────────────────────────────┐
│                          User Code                                  │
│                                                                     │
│  wf := workflow.New(runner, "my-workflow", myFunc)                  │
│  id, _ := wf.Start(ctx, &params)                                   │
│                                                                     │
│  func myFunc(wctx *workflow.Context, p *Params) (*Out, error) {    │
│      result, _ := workflow.Step(wctx, doWork)                       │
│      workflow.Sleep(wctx, 5*time.Minute)                            │
│      data, _ := workflow.AwaitEvent[T](wctx, "order.shipped:123")  │
│      return &Out{...}, nil                                          │
│  }                                                                  │
└──────────────┬──────────────────────────────────────────────────────┘
               │
               ▼
┌──────────────────────────┐     ┌──────────────────────────┐
│         Runner           │────▶│     TaskQueue             │
│                          │     │  (Memory/Sync/Async)      │
│  - repo (ES repository)  │     │                          │
│  - workflows registry    │     │  Enqueue / Poll /        │
│  - waitStore             │     │  Complete / Fail /       │
│  - queue                 │     │  ExtendLease             │
│  - nowFunc (clock)       │     └──────────┬───────────────┘
│  - logger                │                │
└──────────┬───────────────┘                │
           │                                │
           ▼                                ▼
┌──────────────────────────┐     ┌──────────────────────────┐
│   Event Log              │     │     Worker Loop          │
│   (Memory/SQLite/PG)     │     │                          │
│                          │     │  Poll → Lookup → Run →   │
│  WorkflowInstance events │     │  Complete/Fail           │
│  are stored here         │     └──────────────────────────┘
└──────────────────────────┘
```

---

## Core Concepts

### WorkflowInstance (Aggregate)

`WorkflowInstance` is an **event-sourced aggregate** (embeds `aggregate.Base`) that represents a single running workflow. Its state is fully derived from replaying its events.

**Key state:**
| Field | Purpose |
|---|---|
| `id` | Unique instance ID (`wf-<UUIDv7>`) |
| `workflowName` | Registered name for worker lookup |
| `status` | `running` / `waiting` / `completed` / `failed` / `cancelled` |
| `params` | JSON-serialized input parameters |
| `output` | JSON-serialized final result |
| `stepResults` | `map[int][]byte` — cached step results by index |
| `currentStep` | Next step index to execute |
| `attempt` | Current attempt number (1-indexed) |
| `retryStrategy` | Optional retry configuration |
| `cancellationPolicy` | Optional auto-cancel rules |

### Runner

The `Runner` is the central coordinator. It owns:
- An **aggregate repository** for `WorkflowInstance` (backed by an event log)
- A **task queue** for scheduling workflow executions
- A **workflow registry** (`map[string]executableWorkflow`)
- An **event wait store** for `AwaitEvent`/`EmitEvent` coordination
- A configurable clock (`nowFunc`) for testing

**Construction options:**
- `NewRunner(eventLog, logger, opts...)` — generic, in-memory queue by default
- `NewSqliteRunner(db, opts...)` — SQLite event log + in-memory queue
- `NewSqliteRunnerWithSyncQueue(db, opts...)` — **production setup**: SQLite event log + transactional `SyncQueue` (task creation is atomic with event writes)

### Workflow[Params, Output]

A generic type that binds a workflow name to a typed function:

```go
wf := workflow.New[MyParams, MyOutput](runner, "my-workflow", myFunc)
```

This registers the workflow in the runner's internal registry so the worker can look it up by name. The `Workflow` type provides:
- `Start(ctx, params, opts...)` → creates instance, saves `workflowStarted` event, enqueues task
- `Run(ctx, instanceID)` → loads instance, replays completed steps, executes remaining steps
- `GetResult(ctx, instanceID)` → retrieves output from a completed instance
- `GetStatus(ctx, instanceID)` → returns status and attempt info

### Worker Loop

`Runner.RunWorker(ctx, opts)` starts a blocking poll loop:

```
loop:
  1. Poll queue for ready task
  2. Look up workflow by name in registry
  3. Call workflow.execute(ctx, instanceID)
     → internally calls Run() which replays + executes
  4. Handle result:
     - Success → Complete task
     - ErrWorkflowSleeping → Complete (delayed task already enqueued)
     - ErrWorkflowRetrying → Complete (retry task already enqueued)
     - ErrWorkflowWaiting → Complete (EmitEvent will wake it)
     - ErrWorkflowCancelled → Complete
     - Other error → Fail task
  5. If no tasks ready, sleep for pollInterval (default 200ms)
```

### TaskQueue

The `TaskQueue` interface decouples scheduling from execution:

```go
type TaskQueue interface {
    Enqueue(ctx, task QueuedTask) error  // Add task (optionally delayed via RunAfter)
    Poll(ctx) (*QueuedTask, error)       // Get next ready task (claim it)
    Complete(ctx, instanceID) error       // Mark done
    Fail(ctx, instanceID) error           // Mark failed
    ExtendLease(ctx, instanceID, dur) error // Keep-alive for long steps
}
```

### Steps

**`Step[Result]`** — Executes a function and caches the result:
1. Claims a step index from the local counter
2. Reloads the instance from the repository
3. If `stepResults[index]` exists → return cached result (replay)
4. Otherwise → execute function, record `stepCompleted` or `stepFailed`, save

**`Step2`** — Same as Step but for void functions (returns only error).

Step results are JSON-serialized and stored in the event log, making them durable across crashes.

---

## Event Model

All workflow state transitions are captured as events:

| Event | Trigger | Effect |
|---|---|---|
| `workflowStarted` | `wf.Start()` | Initializes instance with params, retry/cancel config |
| `stepCompleted` | `Step()` / `Step2()` / `Sleep()` | Records step result (or sleep wake time) |
| `stepFailed` | Step function returns error | Marks step as failed |
| `workflowCompleted` | Workflow function returns successfully | Records final output |
| `workflowFailed` | Workflow fails with no retries left | Records error message |
| `workflowRetried` | Auto-retry after failure | Increments attempt, schedules delayed re-run |
| `workflowWaiting` | `AwaitEvent()` | Records event name being waited for |
| `workflowEventReceived` | `AwaitEvent()` replay after `EmitEvent()` | Records received payload |
| `workflowCancelled` | `CancelWorkflow()` or auto-cancel policy | Terminal state |

---

## Execution Flow

### Starting a Workflow

```
wf.Start(ctx, params)
  │
  ├─ Create new WorkflowInstance (NewEmpty)
  ├─ Record workflowStarted event
  ├─ Save to repository (→ event log)
  ├─ Enqueue task to TaskQueue
  └─ Return instanceID
```

### Executing Steps

```
wf.Run(ctx, instanceID)
  │
  ├─ repo.Get(instanceID)           ← replay all events to rebuild state
  ├─ Check status (completed? failed? cancelled?)
  ├─ Check cancellation policy
  ├─ Deserialize params
  ├─ Create Context{stepCount: 0}
  ├─ Execute workflow function:
  │   │
  │   ├─ Step(wctx, fn):
  │   │   ├─ stepIndex = stepCount++
  │   │   ├─ repo.Get() to check stepResults[stepIndex]
  │   │   ├─ If cached → return deserialized result (REPLAY)
  │   │   └─ If not → execute fn, save stepCompleted event
  │   │
  │   ├─ Sleep(wctx, duration):
  │   │   ├─ If cached & still sleeping → enqueue delayed task, return ErrWorkflowSleeping
  │   │   ├─ If cached & elapsed → continue
  │   │   └─ If new → save stepCompleted(sleepResult), enqueue delayed task, return ErrWorkflowSleeping
  │   │
  │   └─ AwaitEvent[T](wctx, name):
  │       ├─ If cached & resolved → return payload
  │       ├─ If cached & timed out → return ErrEventTimeout
  │       ├─ If cached & still waiting → check waitStore, return ErrWorkflowWaiting
  │       └─ If new → save workflowWaiting + stepCompleted, register in waitStore
  │
  ├─ On success: save workflowCompleted event
  ├─ On ErrWorkflowSleeping: return (task queue handles wake-up)
  ├─ On ErrWorkflowWaiting: return (EmitEvent handles wake-up)
  └─ On error: attempt retry → or save workflowFailed
```

### Replay & Resume

The **replay mechanism** is the key to crash resilience:

1. When `Run()` is called, the aggregate is loaded from the event log (all events replayed)
2. The workflow function re-executes from the beginning
3. Each `Step`/`Sleep`/`AwaitEvent` call increments a local step counter
4. If `stepResults[index]` already exists → the cached result is returned immediately (no re-execution)
5. When a step index is reached that has no cached result → actual execution begins

This means the workflow function is **deterministic** — completed steps are replayed from cache, and execution resumes exactly where it left off.

---

## Durable Primitives

### Sleep

Durable sleep that survives process restarts:
- Records `stepCompleted` with a `sleepResult{WakeAt, Duration}`
- Enqueues a delayed task (`RunAfter = wakeAt`) on the TaskQueue
- On replay: if `now < wakeAt`, re-enqueues the delayed task and returns `ErrWorkflowSleeping`
- On replay: if `now >= wakeAt`, continues execution normally

### Retry with Backoff

Configurable via `WithRetryStrategy(RetryStrategy{...})` on `Start()`:
- **MaxAttempts** — total attempts including initial (0 = default 5)
- **BaseDelay** — initial backoff delay
- **Factor** — multiplier per failure (2.0 = exponential)
- **MaxDelay** — ceiling for backoff

When a workflow fails and retries remain:
1. Records `workflowRetried` event with next attempt number and scheduled time
2. Enqueues a delayed task for the retry
3. On next Run: the workflow replays from the beginning (all completed steps are cached)

### AwaitEvent / EmitEvent

Inter-workflow (or external) event signaling:

**AwaitEvent[T](wctx, eventName, opts...)**
- Parks the workflow until `eventName` is emitted
- Optional timeout via `AwaitEventOptions{Timeout}`
- Records `workflowWaiting` event, registers in the wait store
- On timeout: records `ErrEventTimeout` as a real workflow error

**EmitEvent(ctx, runner, eventName, payload)**
- Stores the event payload (first-write-wins / idempotent)
- Looks up all waiting workflows in the wait store
- Enqueues immediate tasks for each waiter

**Pre-emit support:** If an event is emitted before a workflow starts waiting for it, the wait store (or SyncQueue in transactional mode) detects the pre-emitted event and creates an immediate task.

### Heartbeat

For long-running steps, `Heartbeat(wctx, duration)` extends the task lease on the queue to prevent other workers from stealing the task.

### Cancellation

**Manual:** `CancelWorkflow(ctx, runner, instanceID, reason)` — records `workflowCancelled` event.

**Automatic via `CancellationPolicy`:**
- `MaxDuration` — cancel if total elapsed time since start exceeds threshold
- `MaxDelay` — cancel if time since last checkpoint exceeds threshold

Checked at the beginning of each `Run()` call.

---

## TaskQueue Implementations

### MemoryQueue

- **Use case:** Single-process, testing
- Simple mutex-protected slice
- `Poll` removes the task from the slice
- No leases, no persistence
- Tasks lost on process restart

### SyncQueue (SQLite, Transactional)

- **Use case:** Production, single-node or multi-worker with shared SQLite
- SQL-backed `workflow_ready_tasks` table
- Implements **both** `TaskQueue` and `aggregate.TransactionalAggregateProcessor`
- **Atomic task creation:** Tasks are inserted/deleted in the **same SQL transaction** as event writes
  - `workflowStarted` → UPSERT immediate task
  - `stepCompleted` (sleep) → UPSERT delayed task
  - `workflowRetried` → UPSERT delayed task
  - `workflowWaiting` → UPSERT delayed task (or check pre-emit)
  - `workflowCompleted` / `workflowFailed` / `workflowCancelled` → DELETE task
- **Lease-based claiming:** `Poll` atomically selects + claims a task with `claimed_by` and `claimed_until_ns`. Expired leases are reclaimed.
- **Crash-safe:** If a worker crashes, the lease expires and the task becomes available to other workers

### AsyncQueue (Eventually Consistent)

- **Use case:** Non-transactional event logs (NATS, DynamoDB, etc.)
- SQL-backed but populated via `event.AsyncProjection` (watches global event stream)
- Same `workflow_ready_tasks` table and claiming semantics as SyncQueue
- **Trade-off:** Task discovery is eventually consistent (delayed by projection poll interval)
- Execution correctness is guaranteed by optimistic concurrency in the event log

---

## Event Wait Store

Two implementations of the `eventWaitStore` interface:

| | In-Memory (`waitStore`) | Persistent (`persistentWaitStore`) |
|---|---|---|
| **Backing** | `sync.Mutex` + maps | SQLite tables (`workflow_waiting_events`, `workflow_emitted_events`) |
| **Use case** | Single-process, MemoryQueue | Multi-process, SyncQueue |
| **Register** | Stores waiter in map; checks pre-emit | No-op (handled by `SyncQueue.Process`) |
| **Emit** | First-write-wins; wakes waiters | First-write-wins via `INSERT OR IGNORE`; queries waiting table |
| **GetEvent** | Lookup by `instanceID:eventName` key | Query `workflow_emitted_events` by name |
| **Durability** | Lost on restart | Survives restarts |

---

## Component Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                       workflow package                        │
│                                                              │
│  ┌──────────┐    ┌───────────────────┐    ┌──────────────┐  │
│  │ Workflow  │───▶│     Runner        │───▶│  TaskQueue   │  │
│  │ [P, O]   │    │                   │    │              │  │
│  │          │    │  ┌─────────────┐  │    │ MemoryQueue  │  │
│  │ Start()  │    │  │ ES Repo for │  │    │ SyncQueue    │  │
│  │ Run()    │    │  │ Workflow-   │  │    │ AsyncQueue   │  │
│  │ GetResult│    │  │ Instance    │  │    └──────────────┘  │
│  └──────────┘    │  └──────┬──────┘  │                      │
│                  │         │         │    ┌──────────────┐  │
│  ┌──────────┐    │         ▼         │    │ eventWait-   │  │
│  │ Worker   │    │  ┌─────────────┐  │───▶│ Store        │  │
│  │ Loop     │◀───│  │ Event Log   │  │    │              │  │
│  │          │    │  │ (chronicle) │  │    │ waitStore    │  │
│  │ Poll →   │    │  └─────────────┘  │    │ persistent-  │  │
│  │ Run →    │    └───────────────────┘    │ WaitStore    │  │
│  │ Complete │                             └──────────────┘  │
│  └──────────┘                                               │
│                                                              │
│  Free functions:                                             │
│  ┌────────────────────────────────────────────────────────┐  │
│  │ Step[R]()  Step2()  Sleep()  AwaitEvent[T]()          │  │
│  │ EmitEvent()  Heartbeat()  CancelWorkflow()            │  │
│  └────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
         │                              │
         ▼                              ▼
┌─────────────────┐          ┌─────────────────────┐
│ chronicle/event  │          │ chronicle/aggregate  │
│                  │          │                      │
│ event.Log        │          │ aggregate.Base       │
│ event.Any        │          │ aggregate.Repository │
│ event.Registry   │          │ RecordEvent()        │
│ event.Transformer│          │ CommitEvents()       │
└─────────────────┘          └─────────────────────┘
```

---

## Data Flow Summary

### Happy Path (Step Execution)
```
Start → workflowStarted event → task enqueued
  → Worker polls → Run() → Step() executes fn
    → stepCompleted event → save
  → Step() executes fn
    → stepCompleted event → save
  → workflowCompleted event → save → task deleted
```

### Sleep
```
Run() → Sleep(5m)
  → stepCompleted{sleepResult{wakeAt}} → save
  → Enqueue task with RunAfter=wakeAt
  → Return ErrWorkflowSleeping → Worker marks task complete

... 5 minutes later ...

Worker polls → task ready → Run()
  → Step() replays cached results
  → Sleep() replays, wakeAt has passed → continue
  → remaining steps execute normally
```

### Retry
```
Run() → Step() succeeds → Step() fails with error
  → stepFailed event → save
  → scheduleRetry() → workflowRetried event → save
  → Enqueue delayed task
  → Return ErrWorkflowRetrying

... backoff delay ...

Worker polls → Run()
  → Step() replays from cache (step 0 OK)
  → Step() re-executes (step 1 — no cache, previous attempt failed)
  → Success → workflowCompleted
```

### AwaitEvent / EmitEvent
```
Run() → AwaitEvent("order.shipped:42")
  → workflowWaiting event → stepCompleted event → save
  → Register in waitStore
  → Return ErrWorkflowWaiting

... later, from another goroutine/process ...

EmitEvent(ctx, runner, "order.shipped:42", payload)
  → Store in waitStore (first-write-wins)
  → Find waiting workflows → enqueue immediate tasks

Worker polls → Run()
  → AwaitEvent replays → checks waitStore → event found!
  → workflowEventReceived event → stepCompleted (resolved) → save
  → Return payload → workflow continues
```
