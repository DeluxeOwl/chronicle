# Types of projections

> [!WARNING] 
> Wall of text ahead 😴

There's a lot of projection types, choosing which fits you best requires some consideration

## By Scope

1. **Per-aggregate projections**
   * Build the current state of a single entity (aggregate root) by replaying its events.
   * Example: reconstructing the current balance of one bank account.
   * ⚠️ In `chronicle`:
     * This kind of projection is handled by the `repo.Get` method

2. **Global projections**
   * Span across many aggregates to build a denormalized or system-wide view.
   * Example: a leaderboard across all players in a game.
   * ⚠️ In `chronicle`:
     * This is handled by the `event.GlobalLog` interface, where you can `ReadAllEvents(...)` and by `event.AsyncProjection`
     * Can also be done by directly querying the backing store of the event log, as seen in [examples/6_projections/main.go](../examples/6_projections/main.go) - where it can be done via sql queries directly.

## By Behavior

1. **Live / Continuous projections**
   * Update in near-real-time as new events are appended.
   * Example: a notification system that reacts to user actions instantly.
   * ⚠️ In `chronicle`:
     * If you don't require durability, you can wrap the `Save(...)` method on the repository to publish to a queue
     * If you require durability, you can use an `*aggregate.TransactionalRepository` to ensure all writes are durable before publishing, or to update the projection directly etc.
     * Or you can use `event.SyncProjection` with most SQL stores and KV stores.

2. **On-demand / Ad-hoc projections**
   * Rebuilt only when needed, often discarded afterward.
   * Example: running an analytics query over a historic event stream.
   * ⚠️ In `chronicle`:
     * Can be done by directly querying the backing store or via an `event.GlobalLog`

3. **Catch-up projections**
   * Rebuild state by replaying past events until caught up with the live stream.
   * Often used when adding new read models after the system is in production.
   * ⚠️ In `chronicle`:
     * Can be done by directly querying the backing store or via an `event.GlobalLog`
     * It will probably require you to "remember" (possibly in a different store) the last processed id, version etc.
       * This is handled by `event.AsyncProjection`
     * You might also be interested in idempotency


## By Data Transformation

1. **Aggregated projections**
   * Summarize events into counts, totals, or other metrics.
   * Example: total sales per day.
   * ⚠️ In `chronicle`:
     * This most definitely requires an `event.GlobalLog` or querying the backing store directly, since you need the events from a group of different aggregates
     * Can be handled by either an `event.SyncProjection` or `event.AsyncProjection`

2. **Materialized views (denormalized projections)**
   * Store data in a form that's ready for querying, often optimized for UI.
   * Example: user profile with last login, recent purchases, and preferences.
   * ⚠️ In `chronicle`:
     * Can be handled by either an `event.SyncProjection` or `event.AsyncProjection`

3. **Policy projections (process managers/sagas)**
   * React to certain event sequences to drive workflows.
   * Example: after "OrderPlaced" and "PaymentReceived," trigger "ShipOrder."
   * ⚠️ In `chronicle`:
     * Can be handled by either an `event.SyncProjection` or `event.AsyncProjection`

## By Consistency Guarantees

1. **Eventually consistent projections (also called asynchronous projections)**
   * Most common - projections lag slightly behind the event stream.
   * ⚠️ In `chronicle`:
     * This happens if you're using the outbox pattern or have any kind of pub/sub mechanism in your code
     * Or handled by `event.AsyncProjection`

2. **Strongly consistent projections (also called synchronous projections)**
   * Rare in event sourcing, but sometimes required for critical counters or invariants. Works really well if the backing store is SQL or KV.
   * ⚠️ In `chronicle`:
     * This is done when the backing store is an `event.TransactionalLog` at it exposes the transaction AND you want to store your projections in the same store
     * Use an `event.SyncProjection`
     * This is a reasonable and not expensive approach if you're using an SQL store (such as postgres)
     * It also makes the system easier to work with

## By How Often You Update

1. **Real-time (push-driven) projections**
   * Updated immediately as each event arrives.
   * Common when users expect low-latency updates (e.g., dashboards, notifications).
   * Mechanism: event handlers/subscribers consume events as they are written.

2. **Near-real-time (micro-batch)**
   * Updated on a short interval (e.g., every few seconds or minutes).
   * Useful when strict immediacy isn't required but throughput matters.
   * Mechanism: stream processors (like Kafka Streams, Kinesis, or NATS) that batch small sets of events.

3. **Batch/offline projections**
   * Rebuilt periodically (e.g., nightly jobs).
   * Useful for reporting, analytics, or expensive transformations.
   * Mechanism: ETL processes or big data jobs that replay a segment of the event log.

4. **Ad-hoc/on-demand projections**
   * Rebuilt only when explicitly requested.
   * Useful when queries are rare or unique (e.g., debugging, exploratory analytics).
   * Mechanism: fetch event history and compute the projection on the fly, often discarded afterward.

## By Mechanism of Updating

1. **Event-synchronous (push)**
   * Event store or message bus pushes new events to projection handlers.
   * Each projection updates incrementally.
   * Example: subscribing to an event stream and applying changes one by one.

2. **Polling (pull)**
   * Projection service periodically polls the event store for new events.
   * Useful when the event store doesn't support subscriptions or you want more control over load.

3. **Replay / Rebuild**
   * Projection is rebuilt from scratch by replaying all relevant events.
   * Often used when introducing a new projection type or after schema changes.

4. **Hybrid**
   * Projection is initially rebuilt via a replay, then switches to real-time subscription.
   * This is the common **catch-up projection** pattern.
