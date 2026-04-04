# Why event sourcing?

> [*"Every system is a log"*](https://news.ycombinator.com/item?id=42813049)

Here are some of the most common benefits cited for event sourcing:
- **Auditing**: You have a complete, unchangeable record of every action that has occurred in your application.
- **Time Travel**: You can reconstruct the state of your application at any point in time.
- **Read/Write Separation**: You can create new read models for new use cases at any time by replaying the event log, without impacting the write side.
- **Scalability**: You can scale the read and write sides of your application independently.
- **Simple integration**: Other systems can subscribe to the event stream.

But the main benefit I agree with comes from [this event-driven.io article](https://event-driven.io/en/dealing_with_eventual_consistency_and_idempotency_in_mongodb_projections/), paraphrasing:
> Event sourcing helps you first model *what happens* (the events), and **then** worry about how to interpret that data using projections.

The event log is a very powerful primitive, from [every system is a log](https://restate.dev/blog/every-system-is-a-log-avoiding-coordination-in-distributed-applications/) (I highly recommend reading this article and discussion on HN to get a better idea why a log is useful):
- Message queues are logs: Apache Kafka, Pulsar, Meta's Scribe are distributed implementations of the log abstraction. 
- Databases (and K/V stores) are logs: changes go to the write-ahead-log first, then get materialized into the tables.
