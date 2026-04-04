# Projections

Projections are your read models, optimized for querying. See [the "what" section for more info](../what-is-event-sourcing.md).

They are **derived** from your event log and can be rebuilt from it (the event log is the source of truth). So in short, projections are and should be treated as disposable.

You generally want to have many specialized projections, instead of a big read model.

This framework isn't opinionated in *how* you're creating projections but provides a few primitives that help.

## Further reading

- [`event.TransactionalEventLog` and `aggregate.TransactionalRepository`](transactional-event-log-and-repository.md)
- [Example](example.md)
- [Example with outbox](example-with-outbox.md)
- [Synchronous Projections (`event.SyncProjection`)](synchronous-projections.md)
- [Asynchronous Projections (`event.AsyncProjection`)](asynchronous-projections.md)
- [Types of projections](types-of-projections.md)
