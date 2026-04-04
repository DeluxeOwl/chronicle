# What is event sourcing?

Event sourcing is a pattern for storing all changes to an application's state as a sequence of *immutable* "**events**".

The current state can be rebuilt from these events, treating the sequence ("**event log**") as the single source of truth.

You can only add new events to the event log; you can never change or delete existing ones.

For example, instead of storing a person's information in a conventional database table (like in PostgreSQL or SQLite):

| id | name | age |
| :--- | :--- | :-: |
| 7d7e974e | John Smith | 2 |
| 44bcdbc3 | Lisa Doe | 44 |

We store a sequence of events in an event log:

| log_id | version | event_name | event_data |
| :--- | :--- | :--- | :--- |
| **person/7d7e974e** | **1** | **person/was_born** | **{"name": "John Smith"}** |
| **person/7d7e974e** | **2** | **person/aged_one_year** | **{}** |
| person/44bcdbc3 | 1 | person/was_born | {"name": "Lisa Doe"} |
| **person/7d7e974e** | **3** | **person/aged_one_year** | **{}** |
| person/44bcdbc3 | 2 | person/aged_one_year | {} |
| ... | ... | ... | ... |

By **applying** (or replaying) these events in order for a, we can reconstruct the current state of any person.

In the example above, you would apply all events with the log ID `person/7d7e974e` (the bold rows), ordered by `version`, to reconstruct the current state for "John Smith".

Let's take a simplified bank account example with the balance stored in an db table:

| id | balance |
| :--- | :--- |
| 162accc9 | $150 |

With this model, if you see the balance is $150, you have no idea *how* it got there. The history is lost.

With event sourcing, the event log would instead store a list of all transactions:

| log_id | version | event_name | event_data |
| :--- | :--- | :--- | :--- |
| account/162accc9 | 1 | account/created | {} |
| account/162accc9 | 2 | account/money_deposited | {"amount": "$200"} |
| account/162accc9 | 3 | account/money_withdrawn | {"amount": "$50"} |

Events are organized per log id (**also called an aggregate id**). In the examples above, you have events **per** person (`person/7d7e974e` and `person/44bcdbc3`) and **per** account (`account/162accc9`).



Events are facts: they describe *something* that happened in the past and should be named in the past tense:
```
person/was_born
person/aged_one_year
account/money_deposited
account/money_withdrawn
```

You might wonder, "What if the user wants to see how many people are named 'John'?" You'd have to replay ALL events for ALL people and count how many have the name "John".

That would be inefficient. This is why **projections** exist.

Projections are specialized **read** models, optimized for querying. They are built by listening to the stream of events as they happen.

Examples of projections:
- How many people are named john
- The people aged 30 to 40
- How much money was withdrawn per day for the past 30 days


Projections can be stored in many different ways, **usually separate** from the event log store itself:
- In a database table you can query with SQL
- In an in-memory database like Redis
- In a search engine like Elasticsearch
- Or simply in the application's memory

For example, let's create a projection that counts people named "John". Our projector is only interested in one event: `person/was_born`. It will ignore all others.

Here's how the projector builds the read model by processing events from the log one by one:

| Incoming Event | Listener's Action | Projection State (our read model) |
| :--- | :--- | :--- |
| *(initial state)* | | `{ "john_count": 0 }` |
| `person/was_born` `{"name": "John Smith"}` | Name starts with "John". `john_count` is incremented. | `{ "john_count": 1 }` |
| `person/aged_one_year` `{}` | Irrelevant event for this projection. State is unchanged. | `{ "john_count": 1 }` |
| `person/was_born` `{"name": "Lisa Doe"}` | Name does not start with "John". State is unchanged. | `{ "john_count": 1 }` |
| `person/was_born` `{"name": "John Doe"}` | Name starts with "John". `john_count` is incremented. | `{ "john_count": 2 }` |
| `person/aged_one_year` `{}` | Irrelevant event for this projection. State is unchanged. | `{ "john_count": 2 }` |
| `person/was_born` `{"name": "Peter Jones"}` | Name does not start with "John". State is unchanged. | `{ "john_count": 2 }` |

The final result is a projection - a simple read model that's fast to query. 
It could be stored in a key-value store like Redis, or a simple db table:

**Table: `name_counts`**

| name_search | person_count |
| :--- | :--- |
| john | 2 |
| lisa | 1 |
| peter | 1 |

Now, when the end user asks, "How many people are named John?", you don't need to scan the entire event log. You simply query your projection, which gives you the answer instantly.

The event log is the source of truth, so projections can be rebuilt from it at any time. However, projections are usually updated *after* an event is written, which means they can briefly lag behind the state in the event log. This is known as **eventual consistency**.

This pattern plays well with Command Query Responsibility Segregation, or CQRS for short:
- **Commands** write to the event log.
- **Queries** read from projections.
