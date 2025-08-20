## What is event sourcing?

Event sourcing is a way to store all changes to some state as a sequence of *immutable* "**events**".

The current state can be rebuilt from events, treating this sequence of events (the "**event log**") as the single source of truth.

You can only add new events to the event log, you can never change or delete existing ones.

Example: instead of storing a person's information in a table in a database (such as postgres, sqlite etc.) 

| id       | name       | age |
| -------- | ---------- | --- |
| 7d7e974e | John Smith | 2   |
| 44bcdbc3 | Lisa Doe   | 44  |

We store a sequence of events in the event log:

| log id              | version | event name               | event data                 |
| ------------------- | ------- | ------------------------ | -------------------------- |
| **person/7d7e974e** | **1**   | **person/was_born**      | **{"name": "John Smith"}** |
| **person/7d7e974e** | **2**   | **person/aged_one_year** | **{}**                     |
| person/44bcdbc3     | 1       | person/was_born          | {"name": "Lisa Doe"}       |
| **person/7d7e974e** | **3**   | **person/aged_one_year** | **{}**                     |
| person/44bcdbc3     | 2       | person/aged_one_year     | {}                         |
| ...                 | ...     | ...                      | ...                        |

By **applying** (or replaying) these events in order (based on the **version**) we can reconstruct the current state of our person.

In the example above, you'd apply all the events in order for the log id `person/7d7e974e` ordered by the version to get the information for "John Smith".

Another simplified example would be a bank account, instead of having

*why do we have $150? we don't know*

| id       | balance |
| -------- | ------- |
| 162accc9 | $150    |

the event log stores a list of all transactions

| log id           | version | event name              | event data         |
| ---------------- | ------- | ----------------------- | ------------------ |
| account/162accc9 | 1       | account/created         | {}                 |
| account/162accc9 | 2       | account/money_deposited | {"amount": "$200"} |
| account/162accc9 | 3       | account/money_withdrawn | {"amount": "$50"}  |


Events are facts: they describe *something* that happened in the past - and they should be named in the past tense:
```
person/was_born
person/aged_one_year
```

You might wonder, what if the end user wants to see how many people are named "John"?
You'd have to apply ALL events for ALL persons and to count how many contain the name "John".

That would be inefficient. That's why **projections** exist.

Projections are specialized **read** models optimized for querying and are built by listening to the stream of events as they happen.

Projections can be stored in a lot of ways, **usually independent** of the event log:
- in a database table that you can query with sql
- in an in-memory db, such as redis
- in elasticsearch
- or simply in memory

Example: a projection that counts people named john
Our projector is only interested in one event: `person/was_born`. It will ignore all others.

Here’s how the projector builds the read model by processing events from the log one by one:

| Incoming Event                                 | Listener's Action                                         | Projection State (our read model) |
| ---------------------------------------------- | --------------------------------------------------------- | --------------------------------- |
| *(initial state)*                              |                                                           | `{ "john_count": 0 }`             |
| `person/was_born` `{"name": "John Smith"}`     | Name starts with "John". `john_count` is incremented.     | `{ "john_count": 1 }`             |
| `person/aged_one_year` `{}`                    | Irrelevant event for this projection. State is unchanged. | `{ "john_count": 1 }`             |
| `person/was_born` `{"name": "Lisa Doe"}`       | Name does not start with "John". State is unchanged.      | `{ "john_count": 1 }`             |
| `person/was_born` `{"name": "John Doe"}`       | Name starts with "John". `john_count` is incremented.     | `{ "john_count": 2 }`             |
| `person/aged_one_year` `{}`                    | Irrelevant event for this projection. State is unchanged. | `{ "john_count": 2 }`             |
| `person/was_born` `{"name": "Peter Jones"}`    | Name does not start with "John". State is unchanged.      | `{ "john_count": 2 }`             |

The final result is a projection, a simple read model that is incredibly fast to query. It could be stored in a a key-value store like Redis, or a simple database table:

**Table: `name_counts`**

| name_search | person_count |
| ----------- | ------------ |
| john        | 2            |
| lisa        | 1            |
| peter       | 1            |


Now, when the end user asks "how many people are named John?", you don't need to scan the entire event log. 
You simply query your projection, which gives you the answer instantly.

The event log is the source of truth from which projections can be rebuilt at any time.
But usually, you have to keep projections up to date, which most often lags behind the write model, also called eventual consistency.

This pattern plays well with Command Query Responsibility Segregation - or CQRS in short:
- the commands **write** to the event log
- the queries **read** from projections

## Why event sourcing?

TODO: event logs are everywhere

Most common benefits that everyone talks about when it comes to event sourcing:
- auditing - you have a record of everything that happens in your application
- time travel - you can reconstruct the state of your application at any point in time
- write/read separation - you can create read models at any time by replaying the event log
- scalability - you can scale the read/write sides of your application independently

But the main benefit I have to agree with comes from [this event-driven.io article](https://event-driven.io/en/dealing_with_eventual_consistency_and_idempotency_in_mongodb_projections/) which states the following:
> Projections have multiple advantages for the development process. The one that I’d like to highlight especially is reducing the cognitive load. You can break down a process into two parts. At first, on modelling the business logic and capturing its result. Then thinking about how to interpret it. This is a liberating experience from thinking all at once in the relational approach.

tl;dr: Event sourcing helps you model first what happens (the events), **then** you use projections to interpret the data.

## Why not event sourcing?

Adopting event sourcing means you have to structure your application around it (it "infects" your application).

In most applications, the current state of the data is all that matters.

Reasons **NOT** to use event sourcing:
- massive overkill for CRUD applications with not a lot of business rules: most often the current state of the data is all that matters
	- e.g. if your events are "PersonCreated", "PersonUpdated", "PersonDeleted", seriously consider avoiding event sourcing
- you don't want to deal with eventual consistency
- some constraints are harder in event sourcing
	- e.g. requiring unique usernames, see TODO
- things like data deletion require complex workarounds
	- e.g. the event log being immutable makes it hard to implement GDPR compliance, requiring things like crypto shedding, see TODO
- it has a high learning curve, most developers aren't familiar with it
	- forcing it on an unprepared team can lead to slow development
- no direct querying for your state
- it often requires external infrastructure, like a queue (nats, amazon sqs, kafka etc.)