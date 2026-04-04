# Projections Example

We're going to make use of `accountv2`. You can find this example in [examples/6_projections/main.go](../examples/6_projections/main.go) and [examples/internal/accountv2/account_processor.go](../examples/internal/accountv2/account_processor.go).

We're going to create a simple projection that is very useful in most event sourced application: a table with the account log ids (that also contains the account holder's name).

We're going to use `sqlite` as the backing event log and as the backing store for our projections.

Why is it useful? Because it shows us a "quick view" of the accounts we have in the system.

In `accountv2/account_processor.go`:
```go
type AccountsWithNameProcessor struct{}
func (p *AccountsWithNameProcessor) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *Account,
	events aggregate.CommittedEvents[AccountEvent],
) error {
	// ...
	return nil
}
```

This is the struct that satisfies our `aggregate.TransactionalAggregateProcessor` interface.

We're going to use a real time, strongly consistent projection, which is possible because we use `sqlite` and the event log and the projections store is the same - which allows us to update the projection in the same transaction.

But first, we need a table for our projection, we'll handle it in the constructor for the `AccountsWithNameProcessor` but can be done outside it as well.

```go
func NewAccountsWithNameProcessor(db *sql.DB) (*AccountsWithNameProcessor, error) {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS projection_accounts (
            account_id TEXT PRIMARY KEY,
            holder_name TEXT NOT NULL
        );
    `)
	if err != nil {
		return nil, fmt.Errorf("new accounts with name processor: %w", err)
	}

	return &AccountsWithNameProcessor{}, nil
}
```

Now what's left to do is to implement our processing logic. 

We're only interested in `*accountOpened` events, from which we extract the root id and the `HolderName`:

```go
func (p *AccountsWithNameProcessor) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *Account,
	events aggregate.CommittedEvents[AccountEvent],
) error {
	for evt := range events.All() {
		// We only care about accountOpened events.
		if opened, ok := evt.(*accountOpened); ok {
			_, err := tx.ExecContext(ctx, `
                INSERT INTO projection_accounts (account_id, holder_name) 
                VALUES (?, ?)
                ON CONFLICT(account_id) DO UPDATE SET 
                    holder_name = excluded.holder_name
            `, root.ID(), opened.HolderName)
			if err != nil {
				return fmt.Errorf("insert account: %w", err)
			}
		}
	}

	return nil
}
```

Let's wire it up in `main`:
```go
func main() {
	db, err := sql.Open("sqlite3", "file:memdb1?mode=memory&cache=shared")
	// ...
	sqlprinter := examplehelper.NewSQLPrinter(db) // We're using a helper to print the tables.

	sqliteLog, err := eventlog.NewSqlite(db)
	// ...
}
```

We can create our processor
```go
	accountProcessor, err := accountv2.NewAccountsWithNameProcessor(db)
```

And hook it up in a `chronicle.NewTransactionalRepository`
```go
	accountRepo, err := chronicle.NewTransactionalRepository(
			sqliteLog,
			accountMaker,
			nil,
			aggregate.NewProcessorChain(
				accountProcessor,
			), // Our transactional processor.
		)
```

`aggregate.NewProcessorChain` is a helper that runs multiple processors one after the other

Let's open an account for Alice and Bob
```go
	// Alice's account
	accA, _ := accountv2.Open(accountv2.AccountID("alice-account-01"), timeProvider, "Alice")
	_ = accA.DepositMoney(100)
	_ = accA.DepositMoney(50)
	_, _, err = accountRepo.Save(ctx, accA)

	// Bob's account
	accB, _ := accountv2.Open(accountv2.AccountID("bob-account-02"), timeProvider, "Bob")
	_ = accB.DepositMoney(200)
	_, _, err = accountRepo.Save(ctx, accB)
```

And we can use our helper to print the projections table:
```go
sqlprinter.Query("SELECT account_id, holder_name FROM projection_accounts")

┌──────────────────┬─────────────┐
│    ACCOUNT ID    │ HOLDER NAME │
├──────────────────┼─────────────┤
│ alice-account-01 │ Alice       │
│ bob-account-02   │ Bob         │
└──────────────────┴─────────────┘
```

Pretty nice, huh? We can query the projection table however we like.

We can also create materialized views in the database by querying the backing sqlite store. Here's an example of querying all events
```go
fmt.Println("All events:")
sqlprinter.Query(
	"SELECT global_version, log_id, version, event_name, json_extract(data, '$') as data FROM chronicle_events",
)
```

**Note:** we're using JSON encoding and we're using `json_extract(data, '$')` to see the data (saved as `BLOB` in sqlite) in a readable format

Running
```bash
go run examples/6_projections/main.go
```

Prints
```bash
All events:
┌────────────────┬──────────────────┬─────────┬─────────────────────────┬─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ GLOBAL VERSION │      LOG ID      │ VERSION │       EVENT NAME        │                                                                                            DATA                                                                                             │
├────────────────┼──────────────────┼─────────┼─────────────────────────┼─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ 1              │ alice-account-01 │ 1       │ account/opened          │ {"eventID":"0198f03e-4042-723c-884d-a9e84426eb9e","occuredAt":"2025-08-28T13:34:28.29074+03:00","id":"alice-account-01","openedAt":"2025-08-28T13:34:28.290625+03:00","holderName":"Alice"} │
│ 2              │ alice-account-01 │ 2       │ account/money_deposited │ {"eventID":"0198f03e-4042-723d-a335-0f13b54c78cb","occuredAt":"2025-08-28T13:34:28.290757+03:00","amount":100}                                                                              │
│ 3              │ alice-account-01 │ 3       │ account/money_deposited │ {"eventID":"0198f03e-4042-723e-a5e5-b127aec593f2","occuredAt":"2025-08-28T13:34:28.290758+03:00","amount":50}                                                                               │
│ 4              │ bob-account-02   │ 1       │ account/opened          │ {"eventID":"0198f03e-4043-723e-874c-2095ca72515f","occuredAt":"2025-08-28T13:34:28.291034+03:00","id":"bob-account-02","openedAt":"2025-08-28T13:34:28.291034+03:00","holderName":"Bob"}    │
│ 5              │ bob-account-02   │ 2       │ account/money_deposited │ {"eventID":"0198f03e-4043-723f-96db-0c01cbf57d70","occuredAt":"2025-08-28T13:34:28.291036+03:00","amount":200}                                                                              │
└────────────────┴──────────────────┴─────────┴─────────────────────────┴─────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```
