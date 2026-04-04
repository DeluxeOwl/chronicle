# Event logs

- **In-Memory**: `eventlog.NewMemory()`
    - The simplest implementation. It stores all events in memory.
    - **Use Case**: Ideal for quickstarts, examples, and running tests.
    - **Tradeoff**: It is not persistent. All data is lost when the application restarts. It can only be used within a single process.
- **SQLite & PostgreSQL**: `eventlog.NewSqlite(db)` and `eventlog.NewPostgres(db)`
    - These are robust, production-ready implementations that leverage SQL databases for persistence. PostgreSQL is ideal for distributed, high-concurrency applications, while SQLite is a great choice for single-server deployments.
    - **Use Case**: Most of them.
    - **Tradeoff**: It requires you to deal with scaling - so they might not be the best if you're dealing with billions of events per second.

A key feature of these SQL-based logs is how they handle optimistic concurrency. Instead of relying on application-level checks, they use **database triggers** to enforce version consistency.

When you try to append an event, a trigger fires inside the database. It atomically checks if the new event's version is exactly one greater than the stream's current maximum version.

If the versions don't line up, the database transaction itself fails and raises an exception, preventing race conditions.

> [!NOTE] 
> To ensure the framework can correctly identify a conflict regardless of the database driver used, the triggers are designed to raise a specific error message: `_chronicle_version_conflict: <actual_version>`. The framework parses this string to create a `version.ConflictError`, making the conflict driver agnostic.
