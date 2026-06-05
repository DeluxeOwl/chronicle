# Workflow package — architecture map

The workflow package is a **durable, replay-based workflow engine** built on top
of Chronicle's event sourcing primitives. Think Temporal/DBOS, but as a library
backed by your existing event log.

A workflow is just an aggregate (`WorkflowInstance`). Each step a user writes
becomes an event in that aggregate's stream. Restarting a workflow means
**loading the aggregate and replaying events** — completed steps return cached
results instead of running again.

---

## 1. The 30,000-foot view

```
   ┌─────────────────────────────────────────────────────────────────┐
   │                          YOUR CODE                              │
   │                                                                 │
   │   wf := workflow.New(runner, "send-email", func(wctx, p) {      │
   │       a, _ := workflow.Step(wctx, fetchUser)        ← step 0    │
   │       b, _ := workflow.Step(wctx, renderTemplate)   ← step 1    │
   │       workflow.Sleep(wctx, 5*time.Minute)           ← step 2    │
   │       evt := workflow.WaitForEvent[Ack](wctx, "ack:42")  ←  3   │
   │       return result, nil                                        │
   │   })                                                            │
   │   id, _ := wf.Start(ctx, params)   ← enqueues a task            │
   └─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │                          Runner                                 │
   │                                                                 │
   │   ┌────────────┐   ┌────────────────┐   ┌──────────────────┐    │
   │   │ workflows  │   │  Aggregate     │   │   TaskQueue      │    │
   │   │ registry   │   │  Repository    │   │ (Memory / Sync / │    │
   │   │ name → fn  │   │  (event log)   │   │  Async)          │    │
   │   └────────────┘   └────────────────┘   └──────────────────┘    │
   │                          │                       │              │
   │                          ▼                       ▼              │
   │                    event log              ready_tasks table     │
   │                    (the truth)            (work to do now)      │
   │                                                                 │
   │   ┌──────────────────┐                                          │
   │   │ eventWaitStore   │  ← parks WaitForEvent waiters,           │
   │   │ (Memory / Persist)│   resolved by PublishEvent              │
   │   └──────────────────┘                                          │
   └─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
   ┌─────────────────────────────────────────────────────────────────┐
   │                       RunWorker loop                            │
   │                                                                 │
   │   for { task = queue.Poll(); wf.execute(task.InstanceID) }      │
   │                                                                 │
   │   execute() = Run() = load instance → replay → run user fn      │
   └─────────────────────────────────────────────────────────────────┘
```

The two storage planes:

- **Event log** (Chronicle aggregate repo) = source of truth.
  Every step result, sleep deadline, retry decision, etc. is persisted as a
  `WorkflowEvent`.
- **Ready-tasks table** (or in-memory slice) = "what should a worker pick up
  next, and when?" Derived from events.

---

## 2. `WorkflowInstance` — the aggregate

It's just a Chronicle aggregate. Embeds `aggregate.Base`. The events that mutate
it are the workflow's lifecycle events, **not** the user's domain events.

```
                       WorkflowInstance
   ┌─────────────────────────────────────────────────────┐
   │ id            : InstanceID  ("wf-<uuid>")           │
   │ workflowName  : string      ("send-email")          │
   │ status        : running|waiting|completed|          │
   │                 failed|cancelled                    │
   │ params        : []byte      (input JSON)            │
   │ output        : []byte      (return value JSON)     │
   │ stepResults   : map[int][]byte   ← REPLAY CACHE     │
   │ currentStep   : int                                 │
   │ attempt       : int         (1-indexed)             │
   │ retryStrategy : *RetryStrategy                      │
   │ cancellationPolicy : *CancellationPolicy            │
   │ startedAt, lastCheckpointAt : time.Time             │
   └─────────────────────────────────────────────────────┘

   Events that drive Apply():

   workflowStarted         ─→ status=running, attempt=1, save params
   stepCompleted{idx,res}  ─→ stepResults[idx]=res, currentStep++
   stepFailed              ─→ status=failed (later overwritten by
                              workflowFailed or workflowRetried)
   workflowCompleted       ─→ status=completed, save output
   workflowFailed          ─→ status=failed, store error
   workflowRetried{n,...}  ─→ status=running, attempt=n,
                              DELETE stepResults[failedIdx]
                              (so failed step re-runs on replay)
   workflowWaiting         ─→ status=waiting
   workflowEventReceived   ─→ status=running
   workflowCancelled       ─→ status=cancelled
```

The `stepResults` map is the **memoization table** that makes replays cheap and
deterministic. Every `Step` / `Sleep` / `WaitForEvent` is keyed by a
**positional step index**.

---

## 3. Replay: how `Step` works

This is the core trick. `wctx.stepCount` is a local counter that gets bumped
on every step-like call inside the user function.

```
   user function body                  wctx.stepCount   stepResults map
   ─────────────────────────────────   ─────────────    ──────────────────
   Run() loads instance, sets ct=0     ct=0             {0:"alice",1:"hi"}
                                                        (loaded from log)

   Step(wctx, fetchUser)               ct=0→1           HIT  → return "alice"
                                                              from cache

   Step(wctx, renderTemplate)          ct=1→2           HIT  → return "hi"

   Sleep(wctx, 5*time.Minute)          ct=2→3           HIT? wakeAt in past?
                                                          yes → continue
                                                          no  → re-enqueue
                                                                wake task,
                                                                return
                                                                ErrSleeping

   WaitForEvent[Ack](wctx, "ack:42")   ct=3→4           HIT? resolved?
                                                          yes → return ack
                                                          no  → ErrWaiting
                                                          timeout? → record
                                                                step_completed
                                                                + return
                                                                ErrEventTimeout

   return result                       (no event log writes for steps
                                        that hit the cache)
```

When a step **doesn't** hit the cache it does the work, then:

```
   ┌─────────────────────────┐         success           ┌──────────────────┐
   │  fn(wctx.ctx)           │ ──────────────────────→   │ recordThat(      │
   │  user step function     │                           │   stepCompleted) │
   └─────────────────────────┘                           │ + Save           │
            │                                            └──────────────────┘
            │ error
            ▼
   ┌─────────────────────────┐
   │ recordThat(stepFailed)  │
   │ + Save                  │
   │ wctx.lastFailedStep=idx │  ← scheduleRetry reads this
   │ return err              │     so it can clear it on retry
   └─────────────────────────┘
```

> ⚠ Determinism rule: **always call Steps in the same order**.
> The step index is positional, not name-based. The codebase even has a
> separate test file `queue_async_name_test.go` discussing the name embedding
> in events — but the index is still the cache key.

---

## 4. `Run()` — the execution loop on a single task pickup

```
   Run(ctx, instanceID):
   ─────────────────────────────────────────────────────────────────────
   instance ← repo.Get(instanceID)            ← load from event log
                                                 (replays Apply for every
                                                  event since v0)
        │
        ▼
   status terminal?  completed → return cached output
                     failed    → return error
                     cancelled → return ErrWorkflowCancelled
        │
        ▼
   cancellation policy elapsed? → CancelWorkflow(); ErrWorkflowCancelled
        │
        ▼
   wctx ← Context{instance, stepCount: 0, lastFailedStep: nil}
        │
        ▼
   output, err ← userFn(wctx, params)
        │
        ├── err == nil ────→ recordThat(workflowCompleted) + Save → return output
        │
        ├── ErrSleeping ───→ return ErrSleeping       (worker treats as success;
        │                                              wake task already enqueued)
        │
        ├── ErrWaiting ────→ return ErrWaiting        (parked until publish)
        │
        ├── ErrConflictAbort → return — another worker advanced us
        │
        └── other error ──→ scheduleRetry(instance, err):
                              retryStrategy?  no  → recordThat(workflowFailed)
                                                    + Save → return err
                                              yes → recordThat(workflowRetried)
                                                    + Save
                                                    + enqueue delayed task
                                                    → return ErrWorkflowRetrying
```

The instance is **loaded once per Run**, then shared via `wctx.instance` across
all Step/Sleep/WaitForEvent calls. That avoids O(N²) replays inside a single
execution. Each step still hits Save independently — durability after every
checkpoint.

---

## 5. Optimistic concurrency: `ErrConflictAbort`

The event log uses optimistic concurrency (`version.ConflictError`). Two workers
may pick up the same `instanceID` (rare but possible: lease just expired, etc).

```
   Worker A             Worker B
   ────────             ────────
   Poll → task42       Poll → task42  (lease of A expired)
   Run(42)             Run(42)
   Step 5 …            Step 5 …
   Save (v=5)          Save (v=5) ← VersionConflict!
                       saveOrConflictAbort wraps → ErrConflictAbort
                       worker treats it as success → Complete(task)
```

Everywhere a save happens, `saveOrConflictAbort` translates the conflict into
`ErrConflictAbort`. The Run loop and worker loop both special-case this:
**silently abandon** the current execution.

---

## 6. The TaskQueue abstraction

```
   type TaskQueue interface {
       Enqueue(task)                    ← add (with optional RunAfter)
       Poll() → *task                   ← claim next ready task
       Complete(id)                     ← release on success / sleep / wait
       Fail(id)                         ← release after permanent failure
       ExtendLease(id, d)               ← keep claim alive in long steps
   }
```

Three implementations, each with different durability guarantees:

```
   ┌────────────────┬─────────────────────┬─────────────────────────────┐
   │  Implementation│  Backed by          │  Durability of "task ready" │
   ├────────────────┼─────────────────────┼─────────────────────────────┤
   │ MemoryQueue    │ in-memory slice     │ none — lost on restart      │
   │                │                     │ (event log can repopulate   │
   │                │                     │  if you re-trigger)         │
   ├────────────────┼─────────────────────┼─────────────────────────────┤
   │ SyncQueue      │ SQLite table        │ STRONG — task row written   │
   │                │ workflow_ready_     │ in the SAME tx as the event │
   │                │ tasks               │ log append, via Process()   │
   ├────────────────┼─────────────────────┼─────────────────────────────┤
   │ AsyncQueue     │ SQLite table,       │ EVENTUAL — projection lags  │
   │                │ populated by an     │ behind the event log by     │
   │                │ AsyncProjection on  │ pollInterval; but eventually│
   │                │ the global stream   │ converges                   │
   └────────────────┴─────────────────────┴─────────────────────────────┘
```

### SyncQueue: events → tasks in the SAME transaction

This is the "production" path. `SyncQueue` doubles as a
`TransactionalAggregateProcessor`, which Chronicle calls **inside the same
SQL transaction** that appends events.

```
   Step calls repo.Save(instance)
        │
        ▼
   ┌─────────────────────────────────────────────────────────────┐
   │ BEGIN TX                                                    │
   │   1. eventlog.AppendInTx(...)        — write events         │
   │   2. SyncQueue.Process(tx, root, events):                   │
   │        for each event: insert/delete rows in                │
   │          workflow_ready_tasks                               │
   │          workflow_waiting_events                            │
   │ COMMIT                                                      │
   └─────────────────────────────────────────────────────────────┘
        │
        ▼
   No way for events to exist without their tasks, or vice versa.
```

Event → task mapping (in `SyncQueue.processEvent`):

```
   workflowStarted          → UPSERT immediate task
   stepCompleted (sleep)    → UPSERT delayed task at sleepResult.WakeAt
   stepCompleted (normal)   → no task change
   workflowRetried          → UPSERT delayed task at NextRunAfter
   workflowWaiting          → INSERT waiter row in workflow_waiting_events
                              IF pre-published event exists → UPSERT immediate task
                              ELSE IF deadline set         → UPSERT delayed task
                              ELSE                          → no task (parked)
   workflowEventReceived    → DELETE waiter, UPSERT immediate task
   workflowCompleted/Failed/
   Cancelled                → DELETE task + DELETE all waiters for instance
   stepFailed               → no action (user fn decides retry vs fail)
```

### AsyncQueue: events → tasks via projection (eventually)

For backends that **can't** do TX (NATS, in-memory). `AsyncQueue` implements
`event.AsyncProjection`. An `AsyncProjectionRunner` tails the global event log
and calls `Handle(rec)` for each workflow event, which performs the same
event→task mapping as SyncQueue's `Process`, but **outside** the event-write TX.

```
   event log ──(GlobalReader)──→ AsyncProjectionRunner
                                       │ poll every X ms
                                       ▼
                                  AsyncQueue.Handle(rec)
                                       │
                                       ▼
                                  workflow_ready_tasks (lagged)
```

Trade-off: task discovery is delayed. Correctness is preserved because the event
log's optimistic concurrency still prevents duplicate step execution.

---

## 7. Two ways to wire a Runner

```
   ┌──────────────────────────────────────────────────────────────────────┐
   │  NewRunner(eventLog, logger, opts...)                                │
   │                                                                      │
   │  ┌───────────┐                                                       │
   │  │ event log │ ──┐                                                   │
   │  └───────────┘   │                                                   │
   │                  ▼                                                   │
   │            ES Repository (default)                                   │
   │                  │                                                   │
   │                  ▼                                                   │
   │             MemoryQueue (default; can swap via WithTaskQueue)        │
   │                  │                                                   │
   │                  ▼                                                   │
   │             waitStore (in-memory)                                    │
   │                                                                      │
   │  → Fine for tests / single-process; NOT durable for queue state.     │
   └──────────────────────────────────────────────────────────────────────┘

   ┌──────────────────────────────────────────────────────────────────────┐
   │  NewSqliteRunnerWithSyncQueue(db, opts...)                           │
   │                                                                      │
   │  ┌───────────┐                                                       │
   │  │ sqlite db │ ─┬─→ eventlog.NewSqlite()                             │
   │  └───────────┘ │                                                     │
   │                ├─→ NewSyncQueue() — creates                          │
   │                │     workflow_ready_tasks,                           │
   │                │     workflow_waiting_events,                        │
   │                │     workflow_published_events                       │
   │                │                                                     │
   │                └─→ newPersistentWaitStore()                          │
   │                                                                      │
   │                  ▼                                                   │
   │        chronicle.NewTransactionalRepository(                         │
   │            eventLog, NewEmpty, nil,                                  │
   │            syncQueue ← as TransactionalAggregateProcessor            │
   │        )                                                             │
   │                                                                      │
   │  → Durable, distributed-safe. Atomic events + task creation.         │
   └──────────────────────────────────────────────────────────────────────┘
```

`NewRunner` with `WithTaskQueue(asyncQueue)` is the third combination —
in-memory or NATS event log + AsyncQueue projection for distributed
eventually-consistent task discovery.

---

## 8. Worker loop (`RunWorker`)

```
   RunWorker(ctx, opts):
       loop:
           task ← queue.Poll(ctx)
           if task == nil: sleep pollInterval; continue
           wf   ← workflows[task.WorkflowName]
           if !wf: queue.Fail(); continue

           spawn auto-lease-extender goroutine ──── every leaseExtendInterval
                                                    │       call
                                                    ▼
                                                 queue.ExtendLease(id, 3×interval)

           err ← wf.execute(ctx, task.InstanceID)   ← calls Run()
           cancel lease-extender

           switch err:
              nil                  → queue.Complete()
              ErrConflictAbort     → queue.Complete()   (silent)
              ErrWorkflowSleeping  → queue.Complete()   (new task already enqueued)
              ErrWorkflowRetrying  → queue.Complete()   (retry task enqueued)
              ErrWorkflowWaiting   → queue.Complete()   (will be woken by publish)
              ErrWorkflowCancelled → queue.Complete()
              other                → queue.Fail()       (worker logs, moves on)
```

The auto-lease extender solves the "long step + lease expiry" problem without
the user having to call `Heartbeat` manually (though they still can).

---

## 9. Sleep — durable timer

```
   Sleep(wctx, 5*time.Minute):
       idx = wctx.stepCount++
       if stepResults[idx] exists:                    ← REPLAY
           sr = parse(stepResults[idx])
           if now < sr.WakeAt:
               queue.Enqueue(RunAfter=sr.WakeAt)      ← re-arm wake task
               return ErrWorkflowSleeping
           else:
               return nil                              ← sleep elapsed, continue
       else:                                          ← FIRST RUN
           wakeAt = now + d
           recordThat(stepCompleted{
               StepIndex: idx,
               Result: marshal({WakeAt: wakeAt})
           })
           Save                                       ← durable: wake time in event log
           queue.Enqueue(RunAfter=wakeAt)             ← schedule wake-up
           return ErrWorkflowSleeping
```

Sleep is stored as a regular `stepCompleted` event with a `sleepResult` payload.
That's how `SyncQueue.processEvent` detects "this stepCompleted is a sleep" and
upserts a delayed task. Same trick for AsyncQueue.

---

## 10. WaitForEvent / PublishEvent — durable signals

A workflow can park itself indefinitely on a named event.

```
   WaitForEvent[T](wctx, "order.shipped:42", timeout):

       if stepResults[idx] exists (REPLAY):
           if resolved:        return decoded payload
           if timedOut:        return ErrEventTimeout
           if deadline passed: record timeout step; return ErrEventTimeout
           payload, found = waitStore.GetEvent(instanceID, eventName)
           if found:
               recordThat(workflowEventReceived{...})
               recordThat(stepCompleted{Result: resolvedResult})
               Save
               return decoded payload
           return ErrWorkflowWaiting

       FIRST RUN:
           deadline = now + timeout  (or zero)
           recordThat(workflowWaiting{event, deadline})    ← status=waiting
           recordThat(stepCompleted{idx, unresolved result})
           Save                                            ← TX writes:
                                                            workflow_ready_tasks
                                                            row updated by
                                                            SyncQueue.Process:
                                                              • pre-published? immediate
                                                              • deadline? delayed
                                                              • else: parked (no task)
           waitStore.Register(instanceID, eventName)
           return ErrWorkflowWaiting


   PublishEvent(ctx, runner, "order.shipped:42", payload):

       waiters = waitStore.Publish(eventName, payload)
                  ┌──────────────────────────────────────────────┐
                  │  in-memory waitStore: first-write-wins,      │
                  │   returns slice of registered waiters        │
                  │                                              │
                  │  persistentWaitStore: INSERT OR IGNORE into  │
                  │   workflow_published_events; SELECT all rows │
                  │   from workflow_waiting_events; DELETE them  │
                  └──────────────────────────────────────────────┘
       for w in waiters:
           queue.Enqueue({InstanceID, WorkflowName})  ← immediate wake task
```

### The two `eventWaitStore` implementations

```
            waitStore (in-memory)             persistentWaitStore (SQLite)
            ─────────────────────             ─────────────────────────────
   Register: append to waiters map            no-op
             (or copy to received if          (SyncQueue.Process inserted
              event already published)         the row in the SAME tx as
                                               the workflowWaiting event)

   Publish:  store payload + return           transactional INSERT OR IGNORE
             waiters slice                     into published_events,
                                               SELECT + DELETE from
                                               waiting_events

   GetEvent: lookup instanceID:eventName      lookup eventName globally in
             in `received` map                 workflow_published_events
                                               (first-write-wins is global)
```

The persistent store makes WaitForEvent work **across processes** — Worker A
parks the workflow; Worker B publishes the event; Worker C wakes and resumes.

---

## 11. Retries

```
   user fn returns err   →   Run catches it   →   scheduleRetry(instance, err)

   scheduleRetry:
       strategy ← instance.retryStrategy        (nil → no retry, fall through to failed)
       if attempt >= MaxAttempts: return (false, nil)  → permanent fail
       delay   = strategy.nextDelay(attempt - 1)       (exponential backoff)
       runAfter = now + delay
       recordThat(workflowRetried{
           Attempt: next,
           NextRunAfter: runAfter,
           FailedStepIndex: wctx.lastFailedStep,  ← so Apply can DELETE
                                                    stepResults[idx]
                                                    to allow re-execution
           RetryStrategy: marshal(strategy),       ← carried in the event
                                                    so it survives replays
       })
       Save
       queue.Enqueue(RunAfter=runAfter)
       return (true, nil)
```

`SyncQueue.Process` sees `workflowRetried` and upserts a delayed task at
`NextRunAfter`. `AsyncQueue.Handle` does the same.

> Why is the failed step index cleared from `stepResults`?
> Because on replay, a cached "failure" would otherwise be replayed as success
> (e.g. a `WaitForEvent` cached its timed-out state, which on retry should be
> a fresh wait, not an immediate timeout).

---

## 12. Cancellation

Two flavors:

```
   ┌─────────────────────────────────────────────────────────────────────┐
   │ Manual:   workflow.CancelWorkflow(ctx, runner, instanceID, reason)  │
   │                                                                     │
   │     Get instance → if terminal: no-op                               │
   │                  → record(workflowCancelled) + Save                 │
   │                                                                     │
   │     SyncQueue.Process sees it → DELETE ready_task                   │
   │                              → DELETE all waiting_events for id     │
   └─────────────────────────────────────────────────────────────────────┘

   ┌─────────────────────────────────────────────────────────────────────┐
   │ Automatic: CancellationPolicy {MaxDuration, MaxDelay}               │
   │                                                                     │
   │     Stored in the workflowStarted event (and re-applied on Apply).  │
   │     At the START of every Run(), checkCancellationPolicy(instance): │
   │        • elapsed since startedAt        > MaxDuration → cancel      │
   │        • elapsed since lastCheckpointAt > MaxDelay    → cancel      │
   │     If either fires: CancelWorkflow(...), return ErrWorkflowCancelled│
   └─────────────────────────────────────────────────────────────────────┘
```

---

## 13. Full lifecycle of one instance — end-to-end timeline

```
   t=0   Start(params)
         ┌─────────────────────────────────────────────────────────────┐
         │ recordThat(workflowStarted)                                 │
         │ repo.Save(instance)        ─→  event log: [started]         │
         │                            ─→  SyncQueue.Process inserts    │
         │                                immediate task               │
         └─────────────────────────────────────────────────────────────┘
         queue.Enqueue({immediate})  (also called explicitly in Start)

   t=1   Worker A: queue.Poll → task
         wf.execute → Run:
           load instance (status=running, stepResults={})
           userFn:
             Step(0, fetchUser)   → run fn   → record(stepCompleted{0})
                                              → Save → tx: event log + task
                                                       row unchanged
             Step(1, render)      → run fn   → record(stepCompleted{1})
                                              → Save
             Sleep(2, 5m)         → record(stepCompleted{2, sleepResult{wakeAt=t+5m}})
                                  → Save → tx: SyncQueue.Process upserts
                                           delayed task RunAfter=t+5m
                                  → queue.Enqueue redundantly (same)
                                  → return ErrSleeping
         worker: ErrSleeping → queue.Complete (no-op for the delayed row,
                               because its claimed_by ≠ worker A's id)

   t=5m  Worker B: queue.Poll → task (RunAfter elapsed)
         wf.execute → Run:
           load instance (replays started, stepCompleted×3)
                       (stepResults={0:user, 1:rendered, 2:sleepResult})
           userFn:
             Step(0) → cache HIT
             Step(1) → cache HIT
             Sleep(2) → cache HIT, wakeAt < now → continue
             WaitForEvent[Ack](3, "ack:42", 1h)
                          → MISS → record(workflowWaiting{deadline=t+1h})
                                   record(stepCompleted{3, unresolved})
                                   Save → tx: insert waiting_event row,
                                              upsert delayed task
                                              (deadline=t+1h)
                                   waitStore.Register
                                   return ErrWaiting
         worker: ErrWaiting → queue.Complete

   t=10m Outside world: PublishEvent(runner, "ack:42", {...})
         waitStore.Publish:
           persistent path:
             INSERT OR IGNORE published_events
             SELECT waiting_events WHERE event_name="ack:42"
             DELETE matched rows
             returns waiters
         queue.Enqueue({immediate}) for each waiter

   t=10m+ε  Worker C: queue.Poll → task
         wf.execute → Run:
           load instance (status=waiting, stepResults still has unresolved
                          waitForEventResult at idx 3)
           userFn:
             Step(0..2)            → cache HIT
             WaitForEvent[Ack](3)  → cache present but Resolved=false
                                     waitStore.GetEvent → found
                                     record(workflowEventReceived)
                                     record(stepCompleted{3, resolved=true})
                                     Save → tx: delete waiting_event row,
                                                upsert immediate task,
                                                (status=running)
                                     return ack payload
             rest of fn …          → eventually produce output
           record(workflowCompleted)
           Save → tx: DELETE ready_task row, DELETE any leftover waiters
         worker: nil → queue.Complete (no-op, already deleted)
```

---

## 14. File map

```
   workflow.go              WorkflowInstance aggregate, events, Apply,
                            Runner, NewRunner / NewSqliteRunner /
                            NewSqliteRunnerWithSyncQueue, Workflow[P,O],
                            Start, Run, Step, Step2, Context
   workflow_sleep.go        Sleep, sleepResult, RunnerOption helpers
   workflow_retry.go        RetryStrategy, scheduleRetry, workflowRetried
   workflow_events.go       WaitForEvent, PublishEvent, in-memory waitStore,
                            workflowWaiting, workflowEventReceived
   workflow_cancel.go       CancelWorkflow, CancellationPolicy, automatic
                            cancellation check
   workflow_heartbeat.go    Heartbeat → queue.ExtendLease

   queue.go                 TaskQueue interface + QueuedTask
   queue_memory.go          MemoryQueue (slice + mutex)
   queue_sync.go            SyncQueue (SQL) + Process (TransactionalAggregateProcessor)
   queue_async.go           AsyncQueue (SQL) + Handle (AsyncProjection)

   wait_store_persistent.go persistentWaitStore (SQL-backed eventWaitStore)
   worker.go                RunWorker loop + auto-lease extension
```

---

## 15. Mental model cheat sheet

- **An instance = an aggregate.** Its event stream IS its execution trace.
- **A step = an event (stepCompleted) keyed by positional index.** Cache for replay.
- **Run() = load aggregate + replay user function.** Cached steps return
  instantly; uncached steps execute and persist.
- **Sleep / WaitForEvent / Retry don't block goroutines.** They persist intent,
  enqueue a future task, return a sentinel error, and the worker frees the slot.
- **The TaskQueue is just "what needs attention right now."** The event log is
  always the source of truth; the queue is reconstructible from it (that's
  exactly what AsyncQueue does).
- **SyncQueue's superpower** is being a `TransactionalAggregateProcessor` —
  it sits inside Chronicle's save TX and writes the ready-tasks row in the
  same atomic commit as the event.
- **Conflict aborts are not failures.** They mean "someone else moved the
  workflow forward." Silently complete the task.
