- [Quickstart](#quickstart)
- [What is event sourcing?](#what-is-event-sourcing)
- [Why event sourcing?](#why-event-sourcing)
- [Why not event sourcing?](#why-not-event-sourcing)
- [Optimistic Concurrency \& Conflict Errors](#optimistic-concurrency--conflict-errors)
	- [Handling conflict errors](#handling-conflict-errors)
	- [Retry with backoff](#retry-with-backoff)
	- [How is this different from SQL transactions?](#how-is-this-different-from-sql-transactions)
	- [Will conflicts be a bottleneck?](#will-conflicts-be-a-bottleneck)
- [Snapshots](#snapshots)
	- [Snapshot strategies](#snapshot-strategies)
- [Ordering: Event log \& Global event log](#ordering-event-log--global-event-log)
- [Storage backends](#storage-backends)
	- [Event logs](#event-logs)
	- [Snapshot stores](#snapshot-stores)
- [Event transformers](#event-transformers)
	- [Example: Crypto shedding for GDPR](#example-crypto-shedding-for-gdpr)


## Quickstart

> [!WARNING]
> I recommend going through the quickstart, since all examples use the `Account` struct from below.

Install the library
```sh
go get github.com/DeluxeOwl/chronicle

# for debugging
go get github.com/sanity-io/litter
```

Define your aggregate and embed `aggregate.Base`. This embedded struct handles the versioning of the aggregate for you.

We'll use a classic yet very simplified bank account example:
```go
package account

import (
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

type Account struct {
	aggregate.Base
}
```

Declare a type for the aggregate's ID. This ID type **MUST** implement `fmt.Stringer`. You also need to add an `ID()` method to your aggregate that returns this ID.

```go
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

The events **MUST** be side effect free (no i/o).
The event methods (`EventName`, `isAccountEvent`) **MUST** have pointer receivers:

```go
// We say an account is "opened", not "created"
type accountOpened struct {
	ID         AccountID `json:"id"`
	OpenedAt   time.Time `json:"openedAt"`
	HolderName string    `json:"holderName"`
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

	openedAt   time.Time
	balance    int // we need to know how much money an account has
	holderName string
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
		a.holderName = event.HolderName
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
func NewEmpty() *Account {
	return new(Account)
}
```

And now, opening an account, and let's say **you can't open an account on a Sunday** (as an example of a business rule):
```go
func Open(id AccountID, currentTime time.Time) (*Account, error) {
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

Getting back to `Open`, recording an event is now straightforward:
```go
func Open(id AccountID, currentTime time.Time) (*Account, error) {
	if currentTime.Weekday() == time.Sunday {
		return nil, errors.New("sorry, you can't open an account on Sunday ¯\\_(ツ)_/¯")
	}

	a := NewEmpty()

	// Note: this is type safe, you'll get autocomplete for the events
	if err := a.recordThat(&accountOpened{
		ID:         id,
		OpenedAt:   currentTime,
		HolderName: holderName,
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
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/account"
	"github.com/sanity-io/litter"
)

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
		account.NewEmpty, // The constructor for our aggregate
		nil,             // This is an optional parameter called "transformers"
	)
	if err != nil {
		panic(err)
	}
```

We create the account and interact with it
```go
	// Create an account
	acc, err := account.Open(AccountID("123"), time.Now(), "John Smith")
	if err != nil {
		panic(err)
	}
	
	// Deposit some money
	err = acc.DepositMoney(200)
	if err != nil {
		panic(err)
	}
	
	// Withdraw some money
	_, err = acc.WithdrawMoney(50)
	if err != nil {
		panic(err)
	}
```

And we use the repo to save the account:
```go
	ctx := context.Background()
	version, committedEvents, err := accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}
```

The repository returns the new version of the aggregate, the list of committed events, and an error if one occurred. The version is also updated on the aggregate instance itself and can be accessed via `acc.Version()` (this is handled by `aggregate.Base`)

An aggregate starts at version 0. The version is incremented for each new event that is recorded.

Printing these values gives:
```go
	fmt.Printf("version: %d\n", version)
	for _, ev := range committedEvents {
		litter.Dump(ev)
	}
```

```go
❯ go run examples/1_quickstart/main.go
version: 3
&main.accountOpened{
  ID: "123",
  OpenedAt: time.Time{}, // Note: litter omits private fields for brevity
  HolderName: "John Smith",
}
&main.moneyDeposited{
  Amount: 200,
}
&main.moneyWithdrawn{
  Amount: 50,
}
```

You can find this example in [./examples/1_quickstart](./examples/1_quickstart).
You can find the implementation of the account in [./examples/internal/account/account.go](./examples/internal/account/account.go).

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

## Optimistic Concurrency & Conflict Errors

You can find this example in [./examples/2_optimistic_concurrency/main.go](./examples/2_optimistic_concurrency/main.go).

What happens if two users try to withdraw money from the same bank account at the exact same time? This is called a "race condition" problem.

Event sourcing handles this using **Optimistic Concurrency Control**. 

`chronicle` handles this for you automatically thanks to the versioning system built into `aggregate.Base` - the struct you embed in your aggregates.

We're gonna use the `Account` example from the `examples/internal/account` package (check the quickstart if you haven't done so).

We're gonna open an account and deposit some money
```go
	acc, _ := account.Open(accID, time.Now(), "John Smith")
	_ = acc.DepositMoney(200) // balance: 200

	_, _, _ = accountRepo.Save(ctx, acc)
	fmt.Printf("Initial account saved. Balance: 200, Version: %d\n\n", acc.Version())
```

The account starts at version 0, `Open` is event 1, `Deposit` is event 2.
After saving, the version will be 2.

Then, we assume two users load the same account at the same time:
```go
	accUserA, _ := accountRepo.Get(ctx, accID)
	fmt.Printf("User A loads account. Version: %d, Balance: %d\n", accUserA.Version(), accUserA.Balance())
	// User A loads account. Version: 2, Balance: 200

	accUserB, _ := accountRepo.Get(ctx, accID)
	fmt.Printf("User B loads account. Version: %d, Balance: %d\n\n", accUserB.Version(), accUserB.Balance())
	// User B loads account. Version: 2, Balance: 200
```

User B tries to withdraw $50:
```go
	_, _ = accUserB.WithdrawMoney(50)
	_, _, _ = accountRepo.Save(ctx, accUserB)
	fmt.Printf("User B withdraws $50 and saves. Account is now at version %d\n", accUserB.Version())
```

User B withdraws $50 and **saves**. Account is now at version 3.

At the **same time**, User A tried to withdraw $100. The business logic passes because their copy of the account *thinks* the balance is still $200.

```go
	_, _ = accUserA.WithdrawMoney(100)
	fmt.Println("User A tries to withdraw $100 and save...")
```

User A tries to save:
```go
	_, _, err := accountRepo.Save(ctx, accUserA)
	if err != nil {
		var conflictErr *version.ConflictError
		if errors.As(err, &conflictErr) {
			fmt.Println("\n💥 Oh no! A conflict error occurred!")
			fmt.Printf("   User A's save failed because it expected version %d, but the actual version was %d.\n",
				conflictErr.Expected, conflictErr.Actual)
		} else {
			// Other save errors
			panic(err)
		}
	}
```

And we get a **conflict error**: `User A's save failed because it expected version 2, but the actual version was 3.`

### Handling conflict errors

How do you handle the error above? The most common way is to **retry the command**:
1. Re-load the aggregate from the repository to get the absolute latest state and version
2. Re-run the original command
3. Try to Save again

```go
	// 💥 Oh no! A conflict error occurred!
	//...
	//... try to withdraw again
	accUserA, _ = accountRepo.Get(ctx, accID)
	_, err = accUserA.WithdrawMoney(100)
	if err != nil {
		panic(err)
	}

	fmt.Println("User A tries to withdraw $100 and save...")
	version, _, err := accountRepo.Save(ctx, accUserA)
	if err != nil {
		panic(err)
	}
	fmt.Printf("User A saved successfully! Version is %d and balance $%d\n", version, accUserA.Balance())
	// User A saved successfully! Version is 4 and balance $50
```



### Retry with backoff
The retry cycle can be handled in a loop. If the conflicts are frequent, you might add a backoff.

You can wrap the repository with `chronicle.NewEventSourcedRepositoryWithRetry`, which uses github.com/avast/retry-go/v4 for retries. 

**The default is to retry 3 times on conflict errors**. You can customize the retry mechanism by providing `retry.Option(s)`.

```go
	import "github.com/avast/retry-go/v4"

	ar, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	accountRepo := chronicle.NewEventSourcedRepositoryWithRetry(ar)
	
	accountRepo := chronicle.NewEventSourcedRepositoryWithRetry(ar, retry.Attempts(5),
		retry.Delay(100*time.Millisecond),    // Initial delay
		retry.MaxDelay(10*time.Second),       // Cap the maximum delay
		retry.DelayType(retry.BackOffDelay),  // Exponential backoff
		retry.MaxJitter(50*time.Millisecond), // Add randomness
	)
```

<details>
<summary>Example of wrapping a repository with a custom `Save` method with retries</summary>

```go
type SaveResult struct {
	Version         version.Version
	CommittedEvents aggregate.CommittedEvents[account.AccountEvent]
}

type SaverWithRetry struct {
	saver aggregate.Saver[account.AccountID, account.AccountEvent, *account.Account]
}

func (s *SaverWithRetry) Save(ctx context.Context, root *account.Account) (version.Version, aggregate.CommittedEvents[account.AccountEvent], error) {
	result, err := retry.DoWithData(
		func() (SaveResult, error) {
			version, committedEvents, err := s.saver.Save(ctx, root)
			if err != nil {
				return SaveResult{}, err
			}
			return SaveResult{
				Version:         version,
				CommittedEvents: committedEvents,
			}, nil
		},
		retry.Attempts(3),
		retry.Context(ctx),
		retry.RetryIf(func(err error) bool {
			// Only retry on ConflictErr or specific errors
			var conflictErr *version.ConflictError
			return errors.As(err, &conflictErr)
		}),
	)
	if err != nil {
		var zero version.Version
		var zeroCE aggregate.CommittedEvents[account.AccountEvent]
		return zero, zeroCE, err
	}

	return result.Version, result.CommittedEvents, nil
}
// ...
	accountRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	repoWithRetry := &aggregate.FusedRepo[account.AccountID, account.AccountEvent, *account.Account]{
		AggregateLoader: accountRepo,
		VersionedGetter: accountRepo,
		Getter:          accountRepo,
		Saver: &SaverWithRetry{
			saver: accountRepo,
		},
	}
```
</details>

### How is this different from SQL transactions?

SQL transactions often use **pessimistic locking** - which means you assume a conflict is likely and you lock the data upfront to prevent it:

1. **`BEGIN TRANSACTION;`**: Start a database transaction.
2. **`SELECT balance FROM accounts WHERE id = 'acc-123' FOR UPDATE;`**: This is the critical step. The `FOR UPDATE` clause tells the database to place a **write lock** on the selected row.
3. While this lock is held, no other transaction can read that row with `FOR UPDATE` or write to it. Any other process trying to do so will be **blocked** and forced to wait until the first transaction is finished.
4. The application code checks if `balance >= amount_to_withdraw`.
5. **`UPDATE accounts SET balance = balance - 50 WHERE id = 'acc-123';`**: The balance is updated.
6. **`COMMIT;`**: The transaction is committed, and the lock on the row is released.

In this model, a race condition like the one in our example is impossible. User B's transaction would simply pause at step 2, waiting for User A's transaction to `COMMIT` or `ROLLBACK`. 

The database itself serializes the access. 

In **optimistic concurrency control** - we shift the responsability from out database to our application.

### Will conflicts be a bottleneck?

A common question is: "Will my app constantly handle conflict errors and retries? Won't that be a bottleneck?".

With well designed aggregates, the answer is **no**. Conflicts are the exception, not the rule.

The most important thing to remember is that version conflicts happen **per aggregate**. 

A `version.ConflictError` for `AccountID("acc-123")` has absolutely no effect on a concurrent operation for `AccountID("acc-456")`. The aggregate itself defines the consistency boundary.

Because aggregates are typically designed to represent a single, cohesive entity that is most often manipulated by a single user at a time (like _your_ shopping cart, or _your_ user profile), the opportunity for conflicts is naturally low. 

This fine-grained concurrency model is what allows event-sourced systems to achieve high throughput, as the vast majority of operations on different aggregates can proceed in parallel without any contention.

## Snapshots

You can find this example in [./examples/3_snapshots/main.go](./examples/3_snapshots/main.go) and [(./examples/internal/account/account_snapshot.go](./examples/internal/account/account_snapshot.go).

During the lifecycle of an application, some aggregates might accumulate a very long list of events.

For example, an account could exist for decades and accumulate thousands or even tens of thousands of transactions.

Loading such an aggregate would require fetching and replaying its entire history from the beginning, which could become a performance bottleneck. (As a rule of thumb, always measure first—loading even a few thousand events is often perfectly acceptable performance-wise).

Snapshots are a performance optimization that solves this problem. 

A snapshot is a serialized copy of an aggregate's state at a specific version. Instead of replaying the entire event history, the system can load the *latest snapshot* and then replay only the events that have occurred _since_ that snapshot was taken.

Let's continue our `Account` example from the quickstart and add snapshot functionality. 

First we need to create our snapshot struct which needs to satisfy the interface `aggregate.Snapshot[TID]` (by implementing `ID() AccountID` and `Version() version.Version`). 

This means it must store the aggregate's ID and version, which are then exposed via the required `ID()` and `Version()` methods.

```go
package account

type Snapshot struct {
	AccountID        AccountID       `json:"id"`
	OpenedAt         time.Time       `json:"openedAt"`
	Balance          int             `json:"balance"`
	HolderName       string          `json:"holderName"`
	AggregateVersion version.Version `json:"version"`
}

func (s *Snapshot) ID() AccountID {
	return s.AccountID
}

func (s *Snapshot) Version() version.Version {
	return s.AggregateVersion
}
```

It's a "snapshot" (a point in time picture) of the `Account`'s state at a given point that can be serialized (JSON by default).

Next, we need a way to convert an `Account` aggregate to an `AccountSnapshot` and back. This is the job of a `Snapshotter`. It acts as a bridge between your live aggregate and its serialized snapshot representation.

We create a type that implements the `aggregate.Snapshotter` interface.

```go
package account

type Snapshotter struct{}

func (s *Snapshotter) ToSnapshot(acc *Account) (*Snapshot, error) {
	return &Snapshot{
		AccountID:        acc.ID(), // Important: save the aggregate's id
		OpenedAt:         acc.openedAt,
		Balance:          acc.balance,
		HolderName:       acc.holderName,
		AggregateVersion: acc.Version(), // Important: save the aggregate's version
	}, nil
}

func (s *Snapshotter) FromSnapshot(snap *Snapshot) (*Account, error) {
	// Recreate the aggregate from the snapshot's data
	acc := NewEmpty()
	acc.id = snap.ID()
	acc.openedAt = snap.OpenedAt
	acc.balance = snap.Balance
	acc.holderName = snap.HolderName

	// ⚠️ The repository will set the correct version on the aggregate's Base
	return acc, nil
}
```

The `ToSnapshot` method captures the current state, and `FromSnapshot` restores it. 

Note that `FromSnapshot` doesn't need to set the version on the aggregate's embedded `Base`; the framework handles this automatically when loading from a snapshot.



Let's wire everything up in `main`:
```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/account"
	"github.com/DeluxeOwl/chronicle/snapshotstore"
)

func main() {
	memoryEventLog := eventlog.NewMemory()
	baseRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	accountSnapshotStore := snapshotstore.NewMemoryStore(
		func() *account.Snapshot { return new(account.Snapshot) },
	)
	// ...
}
```

We create a snapshot store for our snapshots. It needs a constructor for an empty snapshot, used for deserialization.

While the library provides an in-memory snapshot store out of the box, the `aggregate.SnapshotStore` interface makes it straightforward to implement your own persistent store (e.g., using a database table, Redis, or a file-based store).

Then, we wrap the base repository with the snapshotting functionality. 

Now, we need to decide _when_ to take a snapshot. You probably don't want to create one on every single change, as that would be inefficient. The framework provides a flexible `SnapshotStrategy` to define this policy and a builder for various strategies:
```go
	accountSnapshotStore := snapshotstore.NewMemoryStore(
		func() *account.Snapshot { return new(account.Snapshot) },
	)

	accountRepo, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
		baseRepo,
		accountSnapshotStore,
		&account.Snapshotter{},
		aggregate.SnapStrategyFor[*account.Account]().EveryNEvents(3),
	)
	if err != nil {
		panic(err)
	}
```

In our example, we chose to snapshot every 3 events.

Let's issue some commands:
```go
	ctx := context.Background()
	accID := account.AccountID("snap-123")

	acc, err := account.Open(accID, time.Now(), "John Smith") // version 1
	if err != nil {
		panic(err)
	}
	_ = acc.DepositMoney(100) // version 2
	_ = acc.DepositMoney(100) // version 3
	
	// Saving the aggregate with 3 uncommitted events.
	// The new version will be 3.
	// Since 3 >= 3 (our N), the strategy will trigger a snapshot.
	_, _, err = accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}
```

The repository loads the snapshot at version 3. Then, it will ask the event log for events for "snap-123" starting from version 4. Since there are none, loading is complete, and very fast.

```go
	reloadedAcc, err := accountRepo.Get(ctx, accID)
	if err != nil {
		panic(err)
	}

	fmt.Printf("Loaded account from snapshot. Version: %d\n", reloadedAcc.Version())
	// Loaded account from snapshot. Version: 3
```

We can also get the snapshot from the store to check:
```go
	snap, found, err := accountSnapshotStore.GetSnapshot(ctx, accID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Found snapshot: %t: %+v\n", found, snap)
	// Found snapshot: true: &{AccountID:snap-123 OpenedAt:2025-08-25 10:53:57.970965 +0300 EEST Balance:200 HolderName:John Smith AggregateVersion:3}
```

### Snapshot strategies
If you type `aggregate.SnapStrategyFor[*account.Account]().` you will get autocomplete for various snapshot strategies:
- `EveryNEvents(n)`: Takes a snapshot every n times the aggregate is saved.
- `AfterCommit()`: Takes a snapshot after every successful save.
- `OnEvents(eventNames...)`: Takes a snapshot only if one or more of the specified event types were part of the save.
- `AllOf(strategies...)`: A composite strategy that triggers only if all of its child strategies match.
- `AnyOf(strategies...)`: A composite strategy that triggers if any of its child strategies match.     

Or a `Custom(...)` strategy, which gives you complete control by allowing you to provide your own function. This function receives the aggregate's state, its versions, and the list of committed events, so you can decide when a snapshot should be taken:
```go
aggregate.SnapStrategyFor[*account.Account]().Custom(
			func(ctx context.Context, root *account.Account, previousVersion, newVersion version.Version, committedEvents aggregate.CommittedEvents[account.AccountEvent]) bool {
				return true // always snapshot
			}),
```

An example in [account_snapshot.go](./examples/internal/account/account_snapshot.go):
```go
func CustomSnapshot(
	ctx context.Context,
	root *Account,
	_, _ version.Version,
	_ aggregate.CommittedEvents[AccountEvent],
) bool {
	return root.balance%250 == 0 // Only snapshot if the balance is a multiple of 250
}
```

**Important**: Regardless of the snapshot strategy chosen, saving the snapshot happens after the new events are successfully committed to the event log. This means the two operations are not atomic. It is possible for the events to be saved successfully but for the subsequent snapshot save to fail. 

By default, an error during a snapshot save will be returned by the Save method. You can customize this behavior with the `aggregate.OnSnapshotError` option, allowing you to log the error and continue, or ignore it entirely.

Since snapshots are purely a performance optimization, ignoring a failed snapshot save can be a safe and reasonable strategy. The aggregate can always be rebuilt from the event log, which remains the single source of truth.

## Ordering: Event log & Global event log

The core guarantee of any event log is that events for a single aggregate are stored and retrieved in the exact order they occurred. When you load `AccountID("123")`, you get an ordered history for that account. This is the contract provided by the base `event.Log` interface.

This is sufficient for rehydrating an aggregate or for building projections that only care about a single stream of events at a time. 

However, some use cases require a guaranteed, chronological order of events across the _entire system_. For example, building a system-wide audit trail or a projection that aggregates data from different types of aggregates requires knowing if `account/opened` for User A happened before or after `order/placed` for User B.

It also makes building the kind of projections where you need multiple aggregates (like seeing total money withdrawn for every `Account`) easier.

This is where the `event.GlobalLog` interface comes in. It extends `event.Log` with the ability to read a single, globally ordered stream of all events. 
Backends like the `Postgres` and `Sqlite` logs implement this by assigning a unique, monotonically increasing `global_version` to every event that is committed, in addition to its per-aggregate `version`.

```go
// event/event_log.go

// GlobalLog extends a standard Log with the ability to read all events across
// all aggregates, in the global order they were committed.
type GlobalLog interface {
    Log
    GlobalReader
}

// GlobalReader defines the contract for reading the global stream.
type GlobalReader interface {
    ReadAllEvents(ctx context.Context, globalSelector version.Selector) GlobalRecords
}
```

> [!IMPORTANT] This distinction exists because not all storage backends can efficiently provide a strict global ordering.

- **SQL Databases**: A database like PostgreSQL or SQLite can easily generate a global order using an `IDENTITY` or `AUTOINCREMENT` primary key on the events table. The provided SQL-based logs implement `event.GlobalLog`.
- **Distributed Systems**: A distributed message queue like Apache Kafka guarantees strict ordering only _within a topic partition_. If each aggregate ID were mapped to a partition, you would have perfect per-aggregate order, but no simple, built-in global order. An event log built on kafka would likely only implement the base `event.Log` interface.
- **Key-Value Stores**: A store like Pebble can also implement `GlobalLog` by maintaining a secondary index for the global order. While reading from this index is fast, the provided `Pebble` implementation makes a trade-off for simplicity: it uses a global lock during writes to safely assign the next `global_version`. This serializes all writes to the event store, which can become a performance bottleneck under high concurrent load.

Chronicle's separate interfaces acknowledge this reality, allowing you to choose a backend that fits your application's consistency and projection requirements. If you need to build projections that rely on the precise, system-wide order of events, you should choose a backend that supports `event.GlobalLog`.

## Storage backends

### Event logs

- **In-Memory**: `eventlog.NewMemory()`
    - The simplest implementation. It stores all events in memory.
    - **Use Case**: Ideal for quickstarts, examples, and running tests.
    - **Tradeoff**: It is not persistent. All data is lost when the application restarts. It can only be used within a single process.
- **Pebble**: `eventlog.NewPebble(db)`
    - A persistent, file-based key-value store - developed by Cockroach Labs.
    - **Use Case**: Where you need persistence without the overhead of a full database server.
    - **Tradeoff**: Like the in-memory store, it is intended for use by a single process at a time. Concurrently writing to the same database files from different processes will lead to corruption. This happens because pebble doesn't have a read-then-write atomic primitive.
- **SQLite & PostgreSQL**: `eventlog.NewSqlite(db)` and `eventlog.NewPostgres(db)`
    - These are robust, production-ready implementations that leverage SQL databases for persistence. PostgreSQL is ideal for distributed, high-concurrency applications, while SQLite is a great choice for single-server deployments.
    - **Use Case**: Most of them.
    - **Tradeoff**: It requires you to deal with scaling - so they might not be the best if you're dealing with billions of events per second.

A key feature of these SQL-based logs is how they handle optimistic concurrency. Instead of relying on application-level checks, they use **database triggers** to enforce version consistency.

When you try to append an event, a trigger fires inside the database. It atomically checks if the new event's version is exactly one greater than the stream's current maximum version.

If the versions don't line up, the database transaction itself fails and raises an exception, preventing race conditions.

> [!NOTE] To ensure the framework can correctly identify a conflict regardless of the database driver used, the triggers are designed to raise a specific error message: `_chronicle_version_conflict: <actual_version>`. The framework parses this string to create a `version.ConflictError`, making the conflict driver agnostic.

### Snapshot stores

- **In-Memory**: `snapshotstore.NewMemoryStore(...)`
    - Stores snapshots in a simple map. Perfect for testing.
    - **Tradeoff**: Single process only, lost on restart.
- **PostgreSQL**: `snapshotstore.NewPostgresStore(...)`
    - A persistent implementation that stores snapshots in a PostgreSQL table using an atomic `INSERT ... ON CONFLICT DO UPDATE` statement.
    - **Use Case**: When you need durable snapshots.

You can create your own snapshot store for other databases (like SQLite or Redis) by implementing the `aggregate.SnapshotStore` interface.

Saving a snapshot is usually an "UPSERT" (update or insert) operation.

## Event transformers
You can find this example in in [./examples/4_transformers_crypto/main.go](./examples/4_transformers_crypto/main.go) and [account_aes_crypto_transformer.go](./examples/internal/account/account_aes_crypto_transformer.go).

Transformers allow you to modify events just before they are serialized and written to the event log, and right after they are read and deserialized. This is a powerful hook for implementing cross-cutting concerns.

Common use cases include:

- **Encryption**: Securing sensitive data within your events.
- **Compression**: Reducing the storage size of large event payloads.
- **Up-casting**: Upgrading older versions of an event to a newer schema on the fly.

When you provide multiple transformers, they are applied in the order you list them for writing, and in the **reverse order** for reading. For example: `encrypt -> compress` on write becomes `decompress -> decrypt` on read.

### Example: Crypto shedding for GDPR

Our account has the holder name field, which we consider PII (Personally Identifiable Information) and we'd like to have it encrypted.
```go
type Account struct {
	aggregate.Base

	id AccountID

	openedAt   time.Time
	balance    int
	holderName string // We assume this is PII (Personally Identifiable Information)
}
```

Generated by the event
```go
type accountOpened struct {
	ID         AccountID `json:"id"`
	OpenedAt   time.Time `json:"openedAt"`
	HolderName string    `json:"holderName"`
}

func (*accountOpened) EventName() string { return "account/opened" }
func (*accountOpened) isAccountEvent()   {}
```

We'll create a `CryptoTransformer` that implements the `event.Transformer[E]` interface to encrypt our . For this example, we'll use AES-GCM.

We define our encrypt and decrypt helper functions in the `account` package. 
```go
package account

func encrypt(plaintext []byte, key []byte) ([]byte, error) {
	// ...
}

func decrypt(ciphertext []byte, key []byte) ([]byte, error) {
	// ...
}
```

And let's define our crypto transformer:
```go
type CryptoTransformer struct {
	key []byte
}

func NewCryptoTransformer(key []byte) *CryptoTransformer {
	return &CryptoTransformer{
		key: key,
	}
}

func (t *CryptoTransformer) TransformForWrite(ctx context.Context, event AccountEvent) (AccountEvent, error) {
	if opened, isOpened := event.(*accountOpened); isOpened {
		fmt.Println("Received \"accountOpened\" event")

		encryptedName, err := encrypt([]byte(opened.HolderName), t.key)
		if err != nil {
			return nil, fmt.Errorf("failed to encrypt holder name: %w", err)
		}
		opened.HolderName = base64.StdEncoding.EncodeToString(encryptedName)

		fmt.Printf("Holder name after encryption and encoding: %s\n", opened.HolderName)
	}

	return event, nil
}
```

We're checking on write if the event is of type `*accountOpened`. If it is, we're encrypting the name and encoding it to base64.

The `TransformForRead` method should be the inverse of this process:
```go
func (t *CryptoTransformer) TransformForRead(ctx context.Context, event AccountEvent) (AccountEvent, error) {
	if opened, isOpened := event.(*accountOpened); isOpened {
		fmt.Printf("Holder name before decoding: %s\n", opened.HolderName)

		decoded, err := base64.StdEncoding.DecodeString(opened.HolderName)
		if err != nil {
			return nil, fmt.Errorf("failed to decode encrypted name: %w", err)
		}

		fmt.Printf("Holder name before decryption: %s\n", decoded)
		decryptedName, err := decrypt(decoded, t.key)
		if err != nil {
			// This happens if the key is wrong (or "deleted")
			return nil, fmt.Errorf("failed to decrypt holder name: %w", err)
		}
		opened.HolderName = string(decryptedName)
		fmt.Printf("Holder name after decryption: %s\n", opened.HolderName)
	}

	return event, nil
}
```

Let's wire everything up:
```go
func main() {
	memoryEventLog := eventlog.NewMemory()

	// A 256-bit key (32 bytes)
	encryptionKey := []byte("a-very-secret-32-byte-key-123456")
	cryptoTransformer := account.NewCryptoTransformer(encryptionKey)
	// ...
}
```

> [!WARNING] In a real production system, you must manage encryption keys securely using a Key Management Service (KMS) like AWS KMS, Google Cloud KMS, HashiCorp Vault or OpenBao. The key should never be hardcoded. You'd also manage one encryption key per aggregate ideally, in our simple example, we're using a hardcoded key for all accounts.

Add the transformer:
```go
	accountRepo, err := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		[]event.Transformer[account.AccountEvent]{cryptoTransformer},
	)
	if err != nil {
		panic(err)
	}
```

Create an account and deposit some money:
```go
	ctx := context.Background()
	accID := account.AccountID("crypto-123")

	acc, err := account.Open(accID, time.Now(), "John Smith")
	if err != nil {
		panic(err)
	}

	_ = acc.DepositMoney(100)
	_, _, err = accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Account for '%s' saved successfully.\n", acc.HolderName())

	// 4. Load the account again to verify decryption
	reloadedAcc, err := accountRepo.Get(ctx, accID)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Account reloaded. Holder name is correctly decrypted: '%s'\n\n", reloadedAcc.HolderName())
```

Running the example prints:
```go
❯ go run examples/4_transformers_crypto/main.go
Received "accountOpened" event
Holder name after encryption and encoding: +hWTqlo9VQBUXBJGFsfJouv5eL3PziMwd+gWhVkbtYbaH1CDbLw=
Account for 'John Smith' saved successfully.
Holder name before decoding: +hWTqlo9VQBUXBJGFsfJouv5eL3PziMwd+gWhVkbtYbaH1CDbLw=
Holder name before decryption: ���Z=UT\F�ɢ��x���#0w��Y��P�l�
Holder name after decryption: John Smith
Account reloaded. Holder name is correctly decrypted: 'John Smith'
```

Finally, let's simulate a GDPR "right to be forgotten" request. We'll "delete" the key and create a new repository with a new transformer using a different key. Attempting to load the aggregate will now fail because the data cannot be decrypted.

```go
	fmt.Println("!!!! Simulating GDPR request: Deleting the encryption key. !!!!")

	deletedKey := []byte("a-very-deleted-key-1234567891234")
	cryptoTransformer = account.NewCryptoTransformer(deletedKey)

	forgottenRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		[]event.Transformer[account.AccountEvent]{cryptoTransformer},
	)
	_, err = forgottenRepo.Get(context.Background(), accID)
	if err != nil {
		fmt.Printf("Success! The data is unreadable. Error: %v\n", err)
	}
```

Running the example again prints:
```go
❯ go run examples/4_transformers_crypto/main.go
Received "accountOpened" event
Holder name after encryption and encoding: +hWTqlo9VQBUXBJGFsfJouv5eL3PziMwd+gWhVkbtYbaH1CDbLw=
Account for 'John Smith' saved successfully.
Holder name before decoding: +hWTqlo9VQBUXBJGFsfJouv5eL3PziMwd+gWhVkbtYbaH1CDbLw=
Holder name before decryption: ���Z=UT\F�ɢ��x���#0w��Y��P�l�
Holder name after decryption: John Smith
Account reloaded. Holder name is correctly decrypted: 'John Smith'

!!!! Simulating GDPR request: Deleting the encryption key. !!!!
Holder name before decoding: +hWTqlo9VQBUXBJGFsfJouv5eL3PziMwd+gWhVkbtYbaH1CDbLw=
Holder name before decryption: ���Z=UT\F�ɢ��x���#0w��Y��P�l�
Success! The data is unreadable. Error: repo get version 0: read and load from store: load from records: read transform for event "account/opened" (version 1) failed: failed to decrypt holder name: cipher: message authentication failed
```