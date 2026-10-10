# Fresh workflow review

## Verdict

The first passing suite did not cover every recovery path.
This second review found additional failures through public APIs and fresh-process startup tests.
The changes correct those failures. They do not establish Temporal parity.

## Confirmed failures and corrections

| Failure | Observable result before the correction | Correction |
| --- | --- | --- |
| A workflow handles a step failure and checkpoints a fallback | Recovery repeats the primary callback and rejects the unchanged recorded path | New failure events replay their outcome until an explicit retry clears that step |
| A cancellation or query helper encounters a read outage inside a step | The calling workflow becomes permanently failed | Helpers preserve the infrastructure error classification |
| A worker starts or cancels another instance | The target write incorrectly requires the invoking worker receipt | Other-stream commands retain context cancellation but exclude the unrelated receipt |
| An async name lookup fails | Projection progress accepts a task named after its instance ID, which no definition can run | New waits include the name. Legacy waits use retained identity. Read errors stop projection progress |
| A destructive legacy queue fails one requeue operation | The only task notification disappears | Workers retry requeue during bounded cleanup and report unrecoverable retention errors |
| A timestamp uses a time zone with second offsets | JSON persistence changes the schedule or lease instant | Queue and workflow timestamps use UTC before persistence |
| A SQLite lease exceeds the signed nanosecond range | Two consecutive claims return different receipts without a clock change | Claim and renewal reject unrepresentable deadlines before mutation |
| Workers initialize a fresh database concurrently | Goose globals race, version tables race, or Postgres catalog constraints fail | Per-call providers, Postgres advisory coordination, and SQLite migration file locks |
| Simultaneous SQLite first opens contend on journal mode | Queue construction returns a lock error before schema creation | Startup retries only SQLite lock errors, with a fixed bound |
| Failure or retry follows an elapsed sleep | The monitor reports an active sleep or assigns the later activity failure time as its completion | Recorded progress completes the sleep without inventing an exact timestamp |

Every failure has a regression test. The tests exercise stored results, scheduling, or public errors rather than handler registration.

## Corrections found while reviewing the fixes

A retry must identify the error that the workflow returns, not the latest failed step.
Callbacks can return the same sentinel, a slice error, or a struct that contains a slice.
Distinct comparable wrappers now identify step invocations and preserve the original error chain during live execution.
An unrelated workflow error does not erase a handled step outcome.

Self-targeted cancellation still requires the worker receipt.
A cross-instance command does not weaken checkpoint fencing for the invoking instance.

The startup barrier now precedes database opening and migration initialization.
This exposed the SQLite first-open lock failure that a post-initialization barrier missed.

Postgres migration-table lookup respects the full search path.
A current version table in `public` does not require DDL permission in an earlier schema.

## Additional distributed scenarios

- Fresh independent processes initialize the same SQLite or Postgres database simultaneously.
- A checkpoint commits, but its COMMIT response never reaches the caller. Replay discovers the saved result without repeating the callback.
- A publication commits, but its response disappears. The original payload and wake-up survive a retry.
- A Postgres server stops during an active workflow. Replacement workers replay the earlier checkpoint and repeat only unfinished work.
- Cancellation fences an active worker while automatic heartbeats keep its claim valid.
- A least-privilege Postgres role opens existing history through a multi-schema search path.

The response-loss proxy withholds a real COMMIT completion packet. It does not simulate rollback.
The database-restart test reopens the same persisted storage. It does not establish replica failover or data-loss guarantees.

The contention test uses a longer lease to tolerate container scheduling pauses.
Dedicated heartbeat, expiry, and stale-checkpoint tests retain their short leases and their stricter ownership assertions.

## Remaining boundaries

- External effects remain at least once. Checkpoint receipts cannot undo an effect in another system.
- Definition versioning and full deterministic replay validation remain absent.
- Replayed failures preserve messages, not arbitrary Go error types or sentinel identities.
  Error-code branches must classify failures inside a step and persist a structured result.
- Older failure events retain their previous behavior. Older workers ignore the new replay flag.
  Mixed worker versions are unsafe for workflows that checkpoint after handled errors.
- Starting another workflow is an external command, not an atomic child-workflow operation.
  A crash before the parent checkpoint can repeat that command.
- A destructive legacy queue cannot recover work after a prolonged requeue outage or process exit.
  Receipt-aware queues remain the deployment choice for recoverable workers.
- SQLite migration locks require a writable local filesystem and the same canonical path.
  Hard-link aliases, separate mount namespaces, and non-cooperating migration tools are outside that guarantee.
  The sidecar file remains on disk. Removing it while another process waits can split lock ownership.
- Windows file locking compiles but lacks runtime verification in this review.
- The shared event-log and snapshot-store migration version-table ordering remains a separate pre-existing limit.
- Activity-specific timeouts, recurring signals, indexed visibility, and independent cancellation timers for parked workflows remain absent.

## Verification

The focused regressions passed during each correction.
The final checks use these commands:

```sh
devbox run -- task test
devbox run -- task lint
devbox run -- go test -race ./workflow/... ./queue ./pkg/migrations -count=3 -shuffle=on -timeout=7m
git diff --check
```

The race command includes the independent-process SQLite and Postgres scenarios.
Individual fault tests retain short deadlines. The overall timeout accommodates three full repetitions with test containers.
