# Chronicle — Developer Map

> A type-safe event sourcing toolkit for Go. This document maps every public API surface (excluding `workflow/`).

## Architecture at a glance

```
Your Aggregate (embed aggregate.Base)
        │
        ▼
  aggregate / chronicle     ← Repositories, Root, Snapshotter
        │
        ▼
      event                 ← Interfaces: Log, Transformer, Registry, Projections
        │
        ▼
      version               ← Version, Selector, ConflictError
```

`eventlog/` and `snapshotstore/` are pluggable implementations of the interfaces above.

---

## How the pieces fit together

### The event log is built from small interfaces

The lowest layer is two tiny interfaces: `Reader` (read a single stream) and `Appender` (write with an optimistic concurrency check). `Log` is just `Reader + Appender` — the minimum viable event log. Everything else is opt-in:

```
Reader + Appender = Log                       ← bare minimum
Log    + GlobalReader = GlobalLog             ← adds cross-stream reads (for projections)
TransactionalLog + Transactor = TransactionalEventLog  ← adds atomic TX support
```

This means a custom backend only needs to implement what it actually supports. NATS implements `Log` + `TransactionalEventLog` but not `GlobalReader` (no global ordering). Postgres and SQLite implement everything. Memory implements everything plus `DeleterLog`.

### Transactions are opt-in

A plain `Log` has no concept of transactions — `AppendEvents` is a single atomic write. If your backend supports real transactions (Postgres, SQLite), it can also implement `TransactionalEventLog[TX]`, which exposes `AppendInTx` and `WithinTx`. This unlocks two things:

1. **`TransactionalRepository`** — saves events + runs a processor (outbox, projection update) in a single TX.
2. **`TransactableLog`** — wraps a transactional log + an optional `SyncProjection`, so the projection runs inside the same TX as the append.

If you don't need transactions, use a plain `ESRepo` with a plain `Log`. No extra complexity.

### Repositories are stackable decorators

The base is `ESRepo` — it loads by replaying events and saves by appending. On top of that, decorators add capabilities:

```
ESRepo                           ← replay all events on load, append on save
  └─ ESRepoWithSnapshots         ← loads from snapshot first, replays only new events
      └─ ESRepoWithRetry         ← retries Save on ConflictError with backoff
```

Each decorator implements the same `Repository` interface, so they compose freely. `FusedRepo` exists for cases where you want to override just one method (e.g. only `Save`) without re-implementing everything.

### Projections: sync vs async

**Sync projections** (`SyncProjection[TX]`) run inside the same transaction that appends events. They see the same TX handle, so updates to your read model are atomic with the event write. Use case: transactional outbox, strongly consistent denormalized tables.

**Async projections** (`AsyncProjection`) poll or tail the global event stream independently. They track progress via a `Checkpointer` and are managed by an `AsyncProjectionRunner`. Use case: eventually consistent read models, cross-service integration, analytics.

### Transformers are a pipeline

Transformers sit between your domain events and the event log. On write, they run in order (e.g. encrypt → compress). On read, they run in reverse (decompress → decrypt). This is the hook for:

- **Encryption / crypto shredding** — encrypt sensitive fields before storage, decrypt on load.
- **Event upcasting** — transform old event shapes into new ones on read (1-to-many supported).
- **Cross-cutting concerns** — `AnyTransformer` works across all event types, adapted per-repo via `AnyTransformerToTyped`.

### The registry connects event names to Go types

When events are stored, only the name (`EventName() string`) and encoded bytes are persisted. The `Registry` maps names back to factory functions (`func() E`) so the framework can decode bytes into concrete Go types. Repos auto-register on creation by default. For cross-aggregate uniqueness, use a shared `AnyEventRegistry`.

---

## `chronicle` (root package)

Convenience re-exports. You wire everything from here.

| Function | What it does |
|---|---|
| `NewEventSourcedRepository(log, createRoot, transformers, ...opts)` | Standard ES repo. JSON by default, auto-registers events. |
| `NewEventSourcedRepositoryWithSnapshots(repo, snapStore, snapshotter, policy, ...opts)` | Wraps a repo with snapshot load/save. |
| `NewEventSourcedRepositoryWithRetry(repo, ...retryOpts)` | Wraps a repo with automatic retry on `ConflictError`. Defaults to 3 attempts. |
| `NewTransactionalRepository(txLog, createRoot, transformers, processor, ...opts)` | Atomic save + processor in one TX. For outbox / sync projections. |
| `NewTransactionalRepositoryWithTransactor(transactor, txLog, createRoot, transformers, processor, ...opts)` | Same, but decouples the transactor from the log. |
| `NewEventRegistry[E]()` | Typed event registry. |
| `NewAnyEventRegistry()` | Type-erased registry for cross-aggregate uniqueness. |

---

## `event/`

### Core types

| Type | Role |
|---|---|
| `Any` | Interface every event must implement: `EventName() string`. |
| `LogID` | `string` typedef — identifies an aggregate's event stream. |
| `Raw` | Encoded event (name + `[]byte` data). Built via `NewRaw(name, data)`. |
| `RawEvents` | `[]Raw`. Has `.All()` iterator, `.ToRecords(logID, startVersion)`. |
| `Record` | Persisted event from a single stream. Fields: `Version()`, `LogID()`, `EventName()`, `Data()`. |
| `GlobalRecord` | Like `Record` + `GlobalVersion()` — position across all streams. |
| `Records` | `iter.Seq2[*Record, error]`. Has `.Collect()` and `.Chan(ctx)`. |
| `GlobalRecords` | Same but for `*GlobalRecord`. |

### Helpers

| Function | What it does |
|---|---|
| `AnyToConcrete[E](event.Any)` | Safe type assertion from `Any` to a concrete type. Returns `(E, bool)`. |

### Event log interfaces

| Interface | Methods | Purpose |
|---|---|---|
| `Reader` | `ReadEvents(ctx, logID, selector) Records` | Read a single stream. |
| `Appender` | `AppendEvents(ctx, logID, versionCheck, rawEvents) (Version, error)` | Write with optimistic concurrency. |
| `Log` | `Reader` + `Appender` | Standard event log. |
| `GlobalLog` | `Log` + `GlobalReader` | Adds `ReadAllEvents(ctx, selector) GlobalRecords`. |
| `GlobalReader` | `ReadAllEvents(...)` | Read the global ordered stream (for projections). |
| `DeleterLog` | `DangerouslyDeleteEventsUpTo(ctx, logID, version)` | ⚠️ Irreversible. Prune old events after snapshotting. |

### Transactional event log

| Interface / Type | Purpose |
|---|---|
| `Transactor[TX]` | `WithinTx(ctx, func(ctx, tx TX) error) error` |
| `TransactionalLog[TX]` | `AppendInTx(ctx, tx, logID, check, events)` + `Reader` |
| `TransactionalEventLog[TX]` | `TransactionalLog` + `Transactor` (e.g. Postgres, SQLite, Memory all implement this) |
| `TransactableLog[TX]` | Wraps a `TransactionalEventLog` + optional `SyncProjection`. Created via `NewLogWithProjection` or `NewTransactableLogWithProjection`. |

### Transformers

Applied in order on write, reverse order on read.

| Type | Purpose |
|---|---|
| `Transformer[E]` | Interface: `TransformForWrite(ctx, []E)` / `TransformForRead(ctx, []E)`. |
| `AnyTransformer` | `Transformer[Any]` — for cross-cutting transformers (encryption, logging). |
| `AnyTransformerToTyped[E](at)` | Adapts an `AnyTransformer` to `Transformer[E]`. |
| `TransformerChain[E]` | Composes multiple transformers. `NewTransformerChain(t1, t2, ...)`. |

Use cases: encryption/decryption, compression, event upcasting (1-to-many).

### Registry

Maps event names → factory functions for decoding.

| Type | Purpose |
|---|---|
| `FuncFor[E]` | `func() E` — factory that creates a zero-value event instance. |
| `FuncsFor[E]` | `[]FuncFor[E]`. Has `.EventNames()`. Returned by your aggregate's `EventFuncs()`. |
| `EventFuncCreator[E]` | Interface your aggregate implements: `EventFuncs() FuncsFor[E]`. |
| `Registry[E]` | `EventRegistrar[E]` + `EventFuncGetter[E]`. |
| `EventRegistry[E]` | Thread-safe map implementation. `NewRegistry[E]()`. |
| `NewConcreteRegistryFromAny[E](anyReg)` | Wraps a global `Registry[Any]` with type-safe access. |
| `GlobalRegistry` | Package-level `*EventRegistry[Any]`. Default backing store for all repos. |

### Projections — Async

| Type | Purpose |
|---|---|
| `AsyncProjection` | Interface: `MatchesEvent(name) bool`, `Handle(ctx, *GlobalRecord) error`. |
| `Checkpointer` | Interface: `GetCheckpoint` / `SaveCheckpoint`. Tracks how far a projection has read. |
| `AsyncProjectionRunner` | Orchestrator. Created via `NewAsyncProjectionRunner(globalLog, checkpointer, projection, name, ...opts)`. Call `.Run(ctx)` to start. |

Runner options:

| Option | Default | What it does |
|---|---|---|
| `WithPollInterval(d)` | 200ms | How often to poll for new events. |
| `WithTailing(bool)` | false | Use blocking reads instead of polling (for NATS-like backends). |
| `WithCheckpointPolicy(p)` | batch-end only | When to persist progress. |
| `WithCancelCheckpointTimeout(d)` | 5s | Grace period for final checkpoint on shutdown. |
| `WithSlogHandler(h)` | `slog.Default()` | Custom logger. |
| `WithSaveCheckpointErrPolicy(fn)` | `StopOnError` | `ContinueOnError` or `StopOnError` or custom. |
| `WithLastCheckpointSaveEnabled(bool)` | true | Save checkpoint on graceful shutdown. |

### Projections — Sync

| Type | Purpose |
|---|---|
| `SyncProjection[TX]` | Interface: `MatchesEvent(name) bool`, `Handle(ctx, tx, []*Record) error`. Runs inside the same TX as the append. |

### Checkpoint policies

| Constructor | Behavior |
|---|---|
| `EveryNEvents(n)` | Checkpoint every N processed events. |
| `AfterDuration(d)` | Checkpoint after time elapsed. |
| `AnyOf(policies...)` | OR logic. |

---

## `aggregate/`

### Building an aggregate

| Type | Role |
|---|---|
| `ID` | Constraint: `fmt.Stringer`. Your aggregate ID type must satisfy this. |
| `Base` | Embed this. Provides `Version()`, internal event tracking, version management. |
| `Root[TID, E]` | Full aggregate contract: `Apply(E) error`, `ID() TID`, `Version()`, `EventFuncs()`. Satisfied by embedding `Base`. |
| `Aggregate[TID, E]` | Subset: `Apply(E) error` + `ID() TID`. |
| `Versioner` | `Version() version.Version`. |

### Recording events

| Function | What it does |
|---|---|
| `RecordEvent(root, event)` | Apply + queue for save. Use in command methods. |
| `RecordEvents(root, events...)` | Apply + queue multiple events at once. |

### Repository interfaces

| Interface | Methods |
|---|---|
| `Getter[TID, E, R]` | `Get(ctx, id) (R, error)` |
| `VersionedGetter[TID, E, R]` | `GetVersion(ctx, id, selector) (R, error)` |
| `Saver[TID, E, R]` | `Save(ctx, root) (Version, CommittedEvents[E], error)` |
| `AggregateLoader[TID, E, R]` | `LoadAggregate(ctx, root, id, selector) error` — load into an existing instance. |
| `Repository[TID, E, R]` | All of the above combined. |
| `FusedRepo[TID, E, R]` | Struct with embedded interfaces. Useful for decorators that override only one method. |

### Repository implementations

| Type | Purpose |
|---|---|
| `ESRepo` | Standard event-sourced repo. Created via `NewESRepo(...)`. |
| `ESRepoWithSnapshots` | Decorator. Loads from snapshot first, replays remaining events. Created via `NewESRepoWithSnapshots(...)`. |
| `ESRepoWithRetry` | Decorator. Retries `Save` on `ConflictError`. Created via `NewESRepoWithRetry(repo, ...retryOpts)`. |
| `TransactionalRepository` | Atomic save + processor in one TX. Created via `NewTransactionalRepository(...)` or `NewTransactionalRepositoryWithTransactor(...)`. |

### ESRepo options

| Option | Effect |
|---|---|
| `EventCodec(codec)` | Override default JSON encoder. |
| `DontRegisterRoot()` | Skip auto-registration (manual or shared registry). |
| `AnyEventRegistry(reg)` | Use a shared `Registry[Any]` for cross-aggregate uniqueness. |

### ESRepoWithSnapshots options

| Option | Effect |
|---|---|
| `OnSnapshotError(fn)` | Custom handler for snapshot save errors. Return `nil` to swallow. |
| `SnapshotSaveEnabled(bool)` | Toggle snapshot saving. Default: `true`. |

### Snapshots

| Type | Role |
|---|---|
| `Snapshot[TID]` | Interface: `ID() TID`, `Version() Version`. Your snapshot struct implements this. |
| `Snapshotter[TID, E, R, TS]` | Interface: `ToSnapshot(R) (TS, error)`, `FromSnapshot(TS) (R, error)`. |
| `SnapshotStore[TID, TS]` | Interface: `SaveSnapshot(ctx, snap)`, `GetSnapshot(ctx, id) (TS, bool, error)`. |
| `LoadFromSnapshot(ctx, store, snapshotter, id)` | Helper. Returns `(R, found, error)`. |

### Snapshot policies

Built via `SnapPolicyFor[R]()` fluent builder:

| Method | Behavior |
|---|---|
| `.EveryNEvents(n)` | Snapshot when version crosses multiples of N. |
| `.OnEvents(names...)` | Snapshot when specific event types are committed. |
| `.AfterCommit()` | Snapshot after every save. Aggressive. |
| `.Custom(fn)` | Full control via a function. |
| `.AnyOf(policies...)` | OR composition. |
| `.AllOf(policies...)` | AND composition. |

### Transactional processing

| Type | Purpose |
|---|---|
| `TransactionalAggregateProcessor[TX, TID, E, R]` | Interface: `Process(ctx, tx, root, CommittedEvents) error`. Runs inside the save TX. |
| `ProcessorChain[TX, TID, E, R]` | Chains multiple processors. `NewProcessorChain(p1, p2, ...)`. |

### Committed / Uncommitted events

| Type | Purpose |
|---|---|
| `UncommittedEvents[E]` | Events applied but not yet saved. |
| `CommittedEvents[E]` | Events that were persisted. Has `.All()` iterator. |
| `FlushUncommittedEvents(root)` | Clears and returns pending events. |
| `CommitEvents(ctx, log, encoder, transformers, root)` | Full save pipeline: flush → encode → append. |
| `CommitEventsWithTX(ctx, transactor, txLog, processor, encoder, transformers, root)` | Same, but within a TX + processor. |

### Low-level helpers

| Function | Purpose |
|---|---|
| `ReadAndLoadFromStore(ctx, root, log, registry, decoder, transformers, id, selector)` | Reads events from the log and hydrates a root. Returns `ErrRootNotFound` if no events. |
| `LoadFromRecords(ctx, root, registry, decoder, transformers, records)` | Hydrates a root from a `Records` iterator. |
| `RawEventsFromUncommitted(ctx, encoder, transformers, uncommitted)` | Encodes uncommitted events to `[]Raw`. |
| `ApplyWriteTransformers(ctx, events, transformers)` | Runs write-side transformer chain. |
| `ApplyReadTransformers(ctx, events, transformers)` | Runs read-side transformer chain (reverse order). |

---

## `version/`

| Type | Purpose |
|---|---|
| `Version` | `uint64` typedef. `Zero = 0`. |
| `Selector{From, To}` | Range for reading events. |
| `SelectFromBeginning` | `Selector{From: 0}` — read all. |
| `SelectFrom(v)` | `Selector{From: v}`. |
| `SelectExact(v)` | `Selector{From: 0, To: v}`. |
| `SelectInterval(from, to)` | `Selector{From: from, To: to}`. |
| `Check` | Interface for optimistic concurrency checks. |
| `CheckExact(v)` | Expects the stream to be exactly at version `v`. Has `.CheckExact(actual) error`. |
| `ConflictError{Expected, Actual}` | Returned when append finds a version mismatch. |
| `NewConflictError(expected, actual)` | Constructor. |

---

## `encoding/`

| Type | Purpose |
|---|---|
| `Codec` | `Encoder` + `Decoder`. |
| `Encoder` | `Encode(v any) ([]byte, error)`. |
| `Decoder` | `Decode(data []byte, v any) error`. |
| `JSONB` | Default codec. `NewJSONB()`. Also available as `DefaultJSONB`. |
| `Protobuf` | For protobuf events. `NewProtobuf()`. Requires `proto.Message`. |
| `Generic` | Function-based codec. `NewGeneric(encodeFn, decodeFn)`. |

---

## `eventlog/` (implementations)

All implementations satisfy `event.Log`, `event.GlobalLog` (except NATS), and `event.TransactionalEventLog[TX]`.

All SQL implementations use DB triggers for optimistic concurrency checks and auto-run migrations on startup.

| Constructor | TX type | Notes |
|---|---|---|
| `NewMemory(...opts)` | `MemTx` | In-memory. `WithMemoryGlobalTailing()` enables blocking reads. Also implements `DeleterLog`. |
| `NewPostgres(db, ...opts)` | `*sql.Tx` | JSONB column. `WithPGMigrations(opts)` to configure. Also implements `DeleterLog`. |
| `NewSqlite(db, ...opts)` | `*sql.Tx` | BLOB column. `WithSqliteMigrations(opts)` to configure. Also implements `DeleterLog`. |
| `NewNATSJetStream(nc, ...opts)` | `jetstream.JetStream` | One stream per aggregate. Atomic batch via ADR-50. No `GlobalReader`. |

NATS options: `WithNATSStreamName`, `WithNATSSubjectPrefix`, `WithNATSStorage`, `WithNATSRetentionPolicy`, `WithReadTimeOut`.

---

## `snapshotstore/` (implementations)

Both satisfy `aggregate.SnapshotStore[TID, TS]`.

| Constructor | Notes |
|---|---|
| `snapshotstore.NewMemory(createSnapshot, ...opts)` | In-memory map. `WithCodec` to override JSON. |
| `snapshotstore.NewPostgres(db, createSnapshot, ...opts)` | Uses `chronicle_snapshots` table with UPSERT. `WithPGMigrations` to configure. |

---

## `pkg/` (utilities)

### `pkg/migrations`

| Type / Function | Purpose |
|---|---|
| `Options{SkipMigrations, Logger}` | Configure migration behavior. |
| `Migrations{DB, Fsys, Logger, Dialect, Dir}` | Input for `RunMigrations`. |
| `RunMigrations(m)` | Runs goose migrations. Used internally by Postgres/SQLite stores. |
| `NopLogger()` | Silent logger for goose. |

### `pkg/timeutils`

| Type | Purpose |
|---|---|
| `TimeProvider` | Interface: `Now() time.Time`. |
| `RealTimeProvider()` | Singleton returning real time. |
| `TimeProviderMock` | Generated mock for testing. |

---

## Sentinel errors

| Error | Where | Meaning |
|---|---|---|
| `aggregate.ErrRootNotFound` | `ReadAndLoadFromStore` | No events found for this aggregate ID. |
| `version.ConflictError` | Any `AppendEvents` / `Save` | Optimistic concurrency violation. |
| `eventlog.ErrNoEvents` | Event log implementations | Attempted to append an empty event slice. |
| `eventlog.ErrUnsupportedCheck` | Event log implementations | Passed a version check type the implementation doesn't support. |
