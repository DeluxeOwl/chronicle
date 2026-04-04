# Snapshot stores

- **In-Memory**: `snapshotstore.NewMemory(...)`
    - Stores snapshots in a simple map. Perfect for testing.
    - **Tradeoff**: Single process only, lost on restart.
- **PostgreSQL**: `snapshotstore.NewPostgres(...)`
    - A persistent implementation that stores snapshots in a PostgreSQL table using an atomic `INSERT ... ON CONFLICT DO UPDATE` statement.
    - **Use Case**: When you need durable snapshots.

You can create your own snapshot store for other databases (like SQLite or Redis) by implementing the `aggregate.SnapshotStore` interface.

Saving a snapshot is usually an "UPSERT" (update or insert) operation.
