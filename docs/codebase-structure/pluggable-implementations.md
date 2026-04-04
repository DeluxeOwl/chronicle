# Pluggable Implementations

These packages provide concrete implementations for the interfaces defined in the core packages. You can choose the ones that exist or create your own.

* `eventlog`: Contains implementations of the `event.Log` interface. The framework ships with several ready to use:
    * `eventlog.NewMemory()`
    * `eventlog.NewSqlite(...)`
    * `eventlog.NewPostgres(...)`
* `snapshotstore`: Contains implementations of the `aggregate.SnapshotStore` interface.
    * `snapshotstore.NewMemory(...)`
    * `snapshotstore.NewPostgres(...)`
