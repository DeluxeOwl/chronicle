# Ordering: Event log & Global event log

The core guarantee of any event log is that events for a single aggregate are stored and retrieved in the exact order they occurred. When you load `AccountID("123")`, you get an ordered history for that account. This is the contract provided by the base `event.Log` interface.

This is sufficient for rehydrating an aggregate or for building projections that only care about a single stream of events at a time. 

However, some use cases require a guaranteed, chronological order of events across the _entire system_. For example, building a system-wide audit trail or a projection that aggregates data from different types of aggregates requires knowing if `account/opened` for User A happened before or after `order/placed` for User B.

It also makes building the kind of projections where you need multiple aggregates (like seeing total money withdrawn for every `Account`) easier.

This is where the `event.GlobalLog` interface comes in. It extends `event.Log` with the ability to read a single, globally ordered stream of all events. 
Backends like the `Postgres` and `Sqlite` logs implement this by assigning a unique, monotonically increasing `global_version` to every event that is committed, in addition to its per-aggregate `version`.

```go
// event/event_log.go

// GlobalLog extends a standard Log with the ability to read all events across
// all aggregates, in the global order they were committed.
type GlobalLog interface {
    Log
    GlobalReader
}

// GlobalReader defines the contract for reading the global stream.
type GlobalReader interface {
    ReadAllEvents(ctx context.Context, globalSelector version.Selector) GlobalRecords
}
```

> [!IMPORTANT] 
> This distinction exists because not all storage backends can efficiently provide a strict global ordering.

- **SQL Databases**: A database like PostgreSQL or SQLite can easily generate a global order using an `IDENTITY` or `AUTOINCREMENT` primary key on the events table. The provided SQL-based logs implement `event.GlobalLog`.
- **Distributed Systems**: A distributed message queue like Apache Kafka guarantees strict ordering only _within a topic partition_. If each aggregate ID were mapped to a partition, you would have perfect per-aggregate order, but no simple, built-in global order. An event log built on kafka would likely only implement the base `event.Log` interface.
- **Key-Value Stores**: A store like Pebble can also implement `GlobalLog` by maintaining a secondary index for the global order. While reading from this index is fast, a `Pebble` implementation might make a trade-off for simplicity: it uses a global lock during writes to safely assign the next `global_version`. This serializes all writes to the event store, which can become a performance bottleneck under high concurrent load. Note: Pebble was removed in [this commit](https://github.com/DeluxeOwl/chronicle/commit/823463288633728a3ea3282384f73ff746b5a866). The reason was that it's inneficient and nobody used it. A better KV store might be bolt.

Chronicle's separate interfaces acknowledge this reality, allowing you to choose a backend that fits your application's consistency and projection requirements. If you need to build projections that rely on the precise, system-wide order of events, you should choose a backend that supports `event.GlobalLog`.
