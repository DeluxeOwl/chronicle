## Quickstart

Install the library
```sh
go get github.com/DeluxeOwl/chronicle

# for debugging
go get github.com/sanity-io/litter
```

Define your aggregate and embed `aggregate.Base`. This embedded struct handles the versioning of the aggregate for you.

We'll use a classic yet very simplified bank account example:
```go
import "github.com/DeluxeOwl/chronicle/aggregate"

type Account struct {
	aggregate.Base
}
```

Declare a type for the aggregate's ID. This ID type **MUST** implement `fmt.Stringer`. You also need to add an `ID()` method to your aggregate that returns this ID.

```go
import "github.com/DeluxeOwl/chronicle/aggregate"

type AccountID string

func (a AccountID) String() string { return string(a) }

type Account struct {
	aggregate.Base

	id AccountID
}

func (a *Account) ID() AccountID {
	return a.id
}
```

Declare the event type for your aggregate using a sum type (we're also using the [go-check-sumtype](https://github.com/alecthomas/go-check-sumtype) linter that comes with [golangci-lint](https://golangci-lint.run/)) for type safety:
```go
//sumtype:decl
type AccountEvent interface {
	event.Any
	isAccountEvent()
}
```

Now declare the events that are relevant for your business domain.
The event methods (`EventName`, `isAccountEvent`) **MUST** have pointer receivers:

```go
// We say an account is "opened", not "created"
type accountOpened struct {
	ID       AccountID `json:"id"`
	OpenedAt time.Time `json:"openedAt"`
}

func (*accountOpened) EventName() string { return "account/opened" }
func (*accountOpened) isAccountEvent()   {}
```

By default, events are serialized to JSON (this can be changed when you configure the repository).

To satisfy the `event.Any` interface (embedded in `AccountEvent`), you must add an `EventName() string` method to each event.

Let's implement two more events:

```go
type moneyDeposited struct {
	Amount int `json:"amount"` // Note: In a real-world application, you would use a dedicated money type instead of an int to avoid precision issues.
}

// ⚠️ Note: the event name is unique
func (*moneyDeposited) EventName() string { return "account/money_deposited" }
func (*moneyDeposited) isAccountEvent()   {}

type moneyWithdrawn struct {
	Amount int `json:"amount"`
}

// ⚠️ Note: the event name is unique
func (*moneyWithdrawn) EventName() string { return "account/money_withdrawn" }
func (*moneyWithdrawn) isAccountEvent()   {}
```



You must now "bind" these events to the aggregate by providing a constructor function for each one. This allows the library to correctly deserialize events from the event log back into their concrete types.

You need to make sure to create a constructor function for each event:

```go
func (a *Account) EventFuncs() event.FuncsFor[AccountEvent] {
	return event.FuncsFor[AccountEvent]{
		func() AccountEvent { return new(accountOpened) },
		func() AccountEvent { return new(moneyDeposited) },
		func() AccountEvent { return new(moneyWithdrawn) },
	}
}
```

Let's go back to the aggregate, and define the fields relevant to our business domain (these fields will be populated when we replay the events):
```go
type Account struct {
	aggregate.Base

	id AccountID

	openedAt time.Time
	balance  int // we need to know how much money an account has
}
```

Now we need a way to build the aggregate's state from its history of events. This is done by "replaying" or "applying" the events to the aggregate.
You shouldn't check business logic rules here, you should just recompute the state of the aggregate.

We'll enforce business rules in commands. 

Note that the event structs themselves are unexported. All external interaction with the aggregate should be done via commands, which in turn generate and record events.

```go
func (a *Account) Apply(evt AccountEvent) error {
	switch event := evt.(type) {
	case *accountOpened:
		a.id = event.ID
		a.openedAt = event.OpenedAt
	case *moneyWithdrawn:
		a.balance -= event.Amount
	case *moneyDeposited:
		a.balance += event.Amount
	default:
		return fmt.Errorf("unexpected event kind: %T", event)
	}
	return nil
}
```

This is type safe with the `gochecksumtype` linter.

If you didn't add any cases, you'd get a linter error:
```
exhaustiveness check failed for sum type "AccountEvent" (from account.go:24:6): missing cases for accountOpened, moneyDeposited, moneyWithdrawn (gochecksumtype)
```

Now, let's actually interact with the aggregate: what can we do with it? what are the **business operations** (commands)?

We can **open an account**, **deposit money** and **withdraw money**.

Let's start with opening an account. This will be a "factory function" that creates and initializes our aggregate.

First, we define a function that returns an empty aggregate, we'll need it later and in the constructor:
```go
func NewEmptyAccount() *Account {
	return new(Account)
}
```

And now, opening an account, and let's say **you can't open an account on a Sunday** (as an example of a business rule):
```go
func OpenAccount(id AccountID, currentTime time.Time) (*Account, error) {
	if currentTime.Weekday() == time.Sunday {
		return nil, errors.New("sorry, you can't open an account on Sunday ¯\\_(ツ)_/¯")
	}
	// ...
}
```

We need a way to "record" this event, for that, we declare a helper, unexported method that uses `RecordEvent` from the `aggregate` package:
```go
func (a *Account) recordThat(event AccountEvent) error {
	return aggregate.RecordEvent(a, event)
}
```

Getting back to `OpenAccount`, recording an event is now straightforward:
```go
func OpenAccount(id AccountID, currentTime time.Time) (*Account, error) {
	if currentTime.Weekday() == time.Sunday {
		return nil, errors.New("sorry, you can't open an account on Sunday ¯\\_(ツ)_/¯")
	}

	a := NewEmptyAccount()

	// Note: this is type safe, you'll get autocomplete for the events
	if err := a.recordThat(&accountOpened{
		ID:       id,
		OpenedAt: currentTime,
	}); err != nil {
		return nil, fmt.Errorf("open account: %w", err)
	}

	return a, nil
}
```

Let's add the other commands for our domain methods - I usually enforce business rules here:
```go
func (a *Account) DepositMoney(amount int) error {
	if amount <= 0 {
		return errors.New("amount must be greater than 0")
	}

	return a.recordThat(&moneyDeposited{
		Amount: amount,
	})
}
```

And withdrawing money:
```go
// Returns the amount withdrawn and an error if any
func (a *Account) WithdrawMoney(amount int) (int, error) {
	if a.balance < amount {
		return 0, fmt.Errorf("insufficient money, balance left: %d", a.balance)
	}

	err := a.recordThat(&moneyWithdrawn{
		Amount: amount,
	})
	if err != nil {
		return 0, fmt.Errorf("error during withdrawal: %w", err)
	}

	return amount, nil
}
```

That's it, it's time to wire everything up.

We start by creating an event log. For this example, we'll use a simple in-memory log, but other implementations (sqlite, postgres etc.) are available.
```go
func main() {
	// Create a memory event log
	memoryEventLog := eventlog.NewMemory()
	//...
}
```

We continue by creating the repository for the accounts:
```go
	accountRepo, err := chronicle.NewEventSourcedRepository(
		memoryEventLog,  // The event log
		NewEmptyAccount, // The constructor for our aggregate
		nil,             // This is an optional parameter called "transformers"
	)
	if err != nil {
		panic(err)
	}
```

We create the account and interact with it
```go
	// Create an account
	account, err := OpenAccount(AccountID("123"), time.Now())
	if err != nil {
		panic(err)
	}
	
	// Deposit some money
	err = account.DepositMoney(200)
	if err != nil {
		panic(err)
	}
	
	// Withdraw some money
	_, err = account.WithdrawMoney(50)
	if err != nil {
		panic(err)
	}
```

And we use the repo to save the account:
```go
	ctx := context.Background()
	version, commitedEvents, err := accountRepo.Save(ctx, account)
	if err != nil {
		panic(err)
	}
```

The repository returns the new version of the aggregate, the list of committed events, and an error if one occurred. The version is also updated on the aggregate instance itself and can be accessed via `account.Version()` (this is handled by `aggregate.Base`)

An aggregate starts at version 0. The version is incremented for each new event that is recorded.

Printing these values gives:
```go
	fmt.Printf("version: %d\n", version)
	for _, ev := range commitedEvents {
		litter.Dump(ev)
	}
```

```go
❯ go run examples/1_quickstart/*.go
version: 3
&main.accountOpened{
  ID: "123",
  OpenedAt: time.Time{}, // Note: litter omits private fields for brevity
}
&main.moneyDeposited{
  Amount: 200,
}
&main.moneyWithdrawn{
  Amount: 50,
}
```

You can find this example in [./examples/1_quickstart](./examples/1_quickstart).

## What is event sourcing?

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

Here’s how the projector builds the read model by processing events from the log one by one:

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

## Why event sourcing?

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
- Message queues are logs: Apache Kafka, Pulsar, Meta’s Scribe are distributed implementations of the log abstraction. 
- Databases (and K/V stores) are logs: changes go to the write-ahead-log first, then get materialized into the tables.

## Why not event sourcing?

Adopting event sourcing is a significant architectural decision; it tends to influence the entire structure of your application (some might say it "infects" it).

In many applications, the current state of the data is all that matters.

Reasons **NOT** to use event sourcing:
- **It can be massive overkill.** For simple CRUD applications without complex business rules, the current state of the data is often all that matters.
    - For example, if your events are just `PersonCreated`, `PersonUpdated`, and `PersonDeleted`, you should seriously consider avoiding event sourcing.
- **You don't want to deal with eventual consistency.** If your application requires immediate, strong consistency between writes and reads, event sourcing adds complexity.
- **Some constraints are harder to enforce.**
    - e.g. requiring unique usernames, see TODO
- **Data deletion and privacy require complex workarounds.**
    - e.g. the event log being immutable makes it hard to implement GDPR compliance, requiring things like crypto shedding, see TODO
- **It has a high learning curve.** Most developers are not familiar with this pattern.
    - Forcing it on an unprepared team can lead to slower development and team friction.
- **You cannot directly query the current state;** you must build and rely on projections for all queries.
- **It often requires additional infrastructure,** such as a message queue (e.g., NATS, Amazon SQS, Kafka) to process events and update projections reliably.