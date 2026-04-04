# The `event.Appender` interface

The `Appender` interface is responsible for writing new events, and its implementation is critical for correctness.

```go
type Appender interface {
	AppendEvents(
		ctx context.Context,
		id LogID,
		expected version.Check,
		events RawEvents,
	) (version.Version, error)
}
```

The most important rule for `AppendEvents` is that the entire operation must be **atomic**. 

Checking the expected version and inserting new events either happens together or not at all.

The `AppendEvents` method receives `event.RawEvents`, which is a slice of events that have already been encoded into bytes. Your job is to persist them.

The overall structure of an `AppendEvents` implementation is nearly identical across different databases. Example for SQL databases:

1. **Start a transaction.**
2. **Get the current version.** Within the transaction, query the database for the highest `version` for the given `log_id`. If no events exist, the current version is `0`.
3. **Perform the concurrency check.** Compare the `actual` version from the database with the `expected` version from the `version.Check` parameter.
4. **Handle conflicts.** If the versions do not match, you must immediately abort the transaction and return a `version.ConflictError`. The `eventlog.parseConflictError` helper can help if you make your database trigger or check constraint produce a specific error message.
    ```go
    if actualVersion != expectedVersion {
        return version.Zero, version.NewConflictError(expectedVersion, actualVersion)
    }
    ```
5. **Prepare records for insertion.** If the check passes, use the `events.ToRecords` helper function. It takes the incoming `RawEvents` and the current version, and it returns a slice of `*event.Record` structs with the correct, incrementing version numbers assigned.
    ```go
    // If actual version is 5, these records will be for versions 6, 7, ...
    recordsToInsert := events.ToRecords(id, actualVersion)
    ```
6. **Insert the new records.** Loop through the slice returned by `ToRecords` and insert each `*event.Record` into your events table. A record has the following exported methods available:
   ```go
	// EventName returns the name of the event from the record.
	func (re *Record) EventName() string

	// Data returns the encoded event payload from the record.
	func (re *Record) Data() []byte

	// LogID returns the identifier of the event stream this record belongs to.
	func (re *Record) LogID() LogID

	// Version returns the sequential version of this event within its log stream.
	func (re *Record) Version() version.Version
   ```
   Note: If your users must follow a specific schema, like [cloudevents](https://cloudevents.io/), you need to enforce it at the encoder (see the `encoding` package) and at the events level (maybe by having a "base" event type). Then you can work with the `[]byte` however you want.
7. **Commit the transaction.**
8. **Return the new version.** The new version will be `actualVersion` plus the number of events you just inserted.
	```go
	newStreamVersion := version.Version(exp) + version.Version(len(events))
	```
