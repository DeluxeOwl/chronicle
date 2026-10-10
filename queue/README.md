# Persistent queue

The `queue` package provides an at-least-once work queue on top of `event.Log`.
Each named queue uses one event-sourced aggregate.
It needs no global reader, projection, queue table, or background scheduler.

A persistent backend preserves the queue across process restarts.
`eventlog.NewMemory()` preserves state only within its process.

## Use

```go
q, err := queue.New[Email](log, "emails")
if err != nil {
    return err
}

err = q.Enqueue(ctx, queue.Message[Email]{
    ID: "welcome/user-123",
    Payload: Email{To: "user@example.com"},
})
if err != nil {
    return err
}

delivery, err := q.Claim(ctx, time.Minute)
if err != nil {
    return err
}
if delivery == nil {
    return nil // No eligible message. Poll again later.
}

if err := sendEmail(ctx, delivery.Message.ID, delivery.Message.Payload); err != nil {
    return q.Nack(ctx, delivery.Receipt, time.Now().Add(time.Minute))
}
return q.Ack(ctx, delivery.Receipt)
```

`log` can be any Chronicle `event.Log`.
Every instance that uses the same backend and queue name shares the queue.
All instances must use the same payload type and codec.
Queue names are encoded into distinct, backend-safe stream IDs.

The complete SQLite example is in [examples/9_persistent_queue](../examples/9_persistent_queue/main.go).
It uses a file-backed database and a stable enqueue ID.

```sh
go run ./examples/9_persistent_queue /tmp/emails.db
```

The first run claims and acknowledges the message.
Subsequent runs do not enqueue that message again.

## Contract

| Operation | Behavior |
| --- | --- |
| `Enqueue` | Persists the ID, encoded payload, and optional `AvailableAt`. |
| `Claim` | Leases the earliest enqueued eligible message and returns its receipt. |
| `Ack` | Acknowledges the message owned by the current, unexpired receipt. |
| `Nack` | Invalidates the receipt and schedules another attempt at `retryAt`. |
| `Extend` | Moves the lease deadline to at least the current time plus the duration. |

`Claim` returns `nil, nil` when no message is eligible.
Scheduled messages and active leases do not block other ready messages.
Lease durations must be positive.
`Extend` never shortens an existing lease.
Zero `AvailableAt` and zero `retryAt` mean immediately eligible.

Each successful claim increments `Delivery.Attempt` and creates a fresh receipt.
At the lease deadline, the message becomes eligible again.
No reaper event or timer is necessary.

A receipt identifies one delivery attempt, not a worker or message.
Expired, unknown, superseded, and acknowledged receipts return `ErrLeaseLost`.
Repeated `Ack` calls also return `ErrLeaseLost` after the first successful acknowledgment.

## Enqueue idempotency

The caller supplies a stable, nonempty message ID with valid UTF-8.
Retrying the same ID, encoded payload, and original `AvailableAt` is a no-op.
This remains true after acknowledgment and after process restart.

Reusing an ID with different contents returns `ErrMessageExists`.
The queue retains a payload fingerprint and original schedule for every accepted ID.
Retries must preserve the original schedule, even after `Nack` changes the current schedule.

A custom codec must produce deterministic bytes for equivalent retries.
Concurrent duplicates use optimistic concurrency to commit at most one enqueue.

## Crash recovery and external effects

A successful enqueue means that the log accepted the message.
A claim returns a delivery only after its event commits.
An unacknowledged message becomes eligible after its lease expires.

**Delivery is at least once, not exactly once.**
A worker can finish an external effect and crash before acknowledgment.
The next worker can repeat that effect.
Use the message ID as an idempotency key in the destination system.

Lease expiry does not stop the old handler.
Receipts prevent stale queue mutations, not external effects from expired workers.
Stop work after `ErrLeaseLost`, and use external fencing where necessary.

A storage error can have an ambiguous outcome.
Retry enqueue with the same ID and contents.
If a claim returns an error, do not execute work from that failed call.
A committed claim with a lost response recovers through lease expiry.

There is no automatic handler loop, retry limit, or dead-letter policy.
The caller owns those policies.
The queue does not make a domain aggregate save and enqueue atomic.
For that requirement, use a transactional processor or outbox.

## Options

- `WithNowFunc(fn)` supplies the clock for eligibility and lease commands.
- `WithCodec(codec)` changes the payload codec. Queue events remain JSON encoded.
- `WithPayloadFactory(fn)` allocates initialized pointer payloads before decoding.
- `WithSnapshots(store, n)` saves queue state every `n` committed events.

The clock, codec, and payload factory must support concurrent calls.
Workers must use synchronized clocks.
Event replay uses recorded state and never reads the clock.

### Protobuf payloads

The built-in Protobuf codec needs a concrete message pointer and an explicit payload factory.
The factory must return a fresh, initialized message for each decode.

```go
q, err := queue.New[*wrapperspb.StringValue](
    log, "messages", queue.WithCodec(encoding.NewProtobuf()),
    queue.WithPayloadFactory(func() *wrapperspb.StringValue {
        return new(wrapperspb.StringValue)
    }),
)
```

A factory can also initialize dynamic Protobuf messages with their descriptor.
With a factory, the codec decodes directly into its returned pointer.
Without a factory, other codecs receive a pointer to the payload slot, `*T`.
For pointer payloads, this slot is a pointer to a pointer.

## Snapshots

```go
store := snapshotstore.NewMemory(func() *queue.Snapshot {
    return new(queue.Snapshot)
})
q, err := queue.New[Email](log, "emails", queue.WithSnapshots(store, 100))
```

Chronicle snapshot stores accept `queue.Snapshot` without a custom snapshotter.
Snapshots preserve pending work, receipts, attempts, schedules, and enqueue fingerprints.
They reduce event replay but do not change concurrency or delivery guarantees.

Snapshot save errors do not fail an already committed queue command.
Snapshot read errors propagate to the caller.
Treat snapshot contents as opaque data.
Keep the complete event history. Event pruning is not part of this primitive.

## Ordering, contention, and retention

Claims follow enqueue order among eligible messages.
Execution and completion order are not strict FIFO.
A scheduled or leased earlier message can run after a later ready message.

All commands on a named queue compete on one stream version.
Conflicts reload state and re-run the command with bounded exponential backoff and jitter.
After 16 conflict retries, the operation returns a wrapped `version.ConflictError`.
Context cancellation interrupts the retry wait.

Without snapshots, each operation replays the complete queue history.
Snapshots reduce replay, but the enqueue fingerprint set still grows with unique IDs.
Use separate named queues or partitions to distribute contention.
For high-throughput discovery, use per-message aggregates with a transactional ready index instead.
