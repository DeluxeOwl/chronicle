# Why not event sourcing?

Adopting event sourcing is a significant architectural decision; it tends to influence the entire structure of your application (some might say it "infects" it).

In many applications, the current state of the data is all that matters.

Reasons **NOT** to use event sourcing:
- **It can be massive overkill.** For simple CRUD applications without complex business rules, the current state of the data is often all that matters.
    - For example, if your events are just `PersonCreated`, `PersonUpdated`, and `PersonDeleted`, you should seriously consider avoiding event sourcing.
- **You don't want to deal with eventual consistency.** If your application requires immediate, strong consistency between writes and reads, event sourcing adds complexity.
- **Some constraints are harder to enforce.**
    - e.g. requiring unique usernames, see TODO
- **Data deletion and privacy require complex workarounds.**
    - e.g. the event log being immutable makes it hard to implement GDPR compliance, requiring things like [crypto shedding](event-transformers/crypto-shedding-gdpr.md).
- **It has a high learning curve.** Most developers are not familiar with this pattern.
    - Forcing it on an unprepared team can lead to slower development and team friction.
- **You cannot directly query the current state;** you must build and rely on projections for all queries.
- **It often requires additional infrastructure,** such as a message queue (e.g., NATS, Amazon SQS, Kafka) to process events and update projections reliably.
