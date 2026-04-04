# Package Dependencies

The dependencies are structured to flow from high level abstractions down to low level details. Your application code depends on `aggregate` or `chronicle` (which re-exports stuff from `aggregate`), which in turn depends on the `event` contracts. The concrete `eventlog` implementations satisfy these contracts.

This design means your domain logic (the aggregate) is completely decoupled from the persistence mechanism (the database).

A simplified view of the dependency flow looks like this:

```
  Your Application Code (e.g., account.Account)
              │
              ▼
┌───────────────────────────┐
│     aggregate/chronicle   │ (Repository, Root, Base, Snapshotter)
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│           event           │ (Log, Any, Registry, Transformer)
└─────────────┬─────────────┘
              │
              ▼
┌───────────────────────────┐
│          version          │ (Version, ConflictError)
└───────────────────────────┘
```

The `eventlog` and `snapshotstore` packages implement interfaces from `event` and `aggregate`:

```
┌───────────────────────┐         ┌───────────────────────────┐
│ eventlog.Postgres     ├────implements───▶ event.Log        │
└───────────────────────┘         └───────────────────────────┘

┌───────────────────────┐         ┌─────────────────────────────────┐
│ snapshotstore.Postgres├────implements──▶ aggregate.SnapshotStore │
└───────────────────────┘         └─────────────────────────────────┘
```
