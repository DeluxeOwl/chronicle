# Example with outbox

> [!NOTE] 
> For a production environment, take a look at https://watermill.io/advanced/forwarder/ .

While strongly-consistent projections (like the one above) are powerful, you often need to notify external systems about events that have occurred. This could involve publishing to a message broker like Kafka/RabbitMQ, calling a third-party webhook, or sending an email. 

A common pitfall is the dual-write problem: what happens if you successfully save the events to your database, but the subsequent call to the message broker fails? The system is now in an inconsistent state. The event happened, but the outside world was never notified.

The Transactional Outbox pattern solves this by leveraging your database's ACID guarantees. The flow is: 
1. Atomically write both the business events (e.g., `accountOpened`) and a corresponding "message to be sent" into your database in a single transaction.
2. A separate, background process polls this "outbox" table for new messages.
3. For each message, it publishes it to the external system (e.g., a message bus).
4. Once successfully published, it deletes the message from the outbox table.

This ensures **at-least-once** delivery. If the process crashes after publishing but before deleting, it will simply re-publish the message on the next run. This makes it a reliable way to integrate with external systems.

We'll build a simple outbox, which will use an in-memory pub/sub channel to act as our message bus.

You can find this example in [examples/7_outbox/main.go](../examples/7_outbox/main.go) and [examples/internal/accountv2/account_outbox_processor.go](../examples/internal/accountv2/account_outbox_processor.go).

First, we create another `TransactionalAggregateProcessor`, the `AccountOutboxProcessor`. Its constructor creates a dedicated outbox table.

```go
func NewAccountOutboxProcessor(db *sql.DB) (*AccountOutboxProcessor, error) {
	_, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS outbox_account_events (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            aggregate_id TEXT NOT NULL,
            event_name TEXT NOT NULL,
            payload BLOB NOT NULL
        );
    `)
	if err != nil {
		return nil, fmt.Errorf("new account outbox processor: could not create table: %w", err)
	}
	return &AccountOutboxProcessor{}, nil
}
```

The Process logic is simple: it encoded each event to JSON and inserts it into the `outbox_account_events` table using the provided transaction `tx`. This guarantees atomicity with the event log persistence.

```go
// Process writes committed AccountEvents to the outbox table within the same transaction.
func (p *AccountOutboxProcessor) Process(
	ctx context.Context,
	tx *sql.Tx,
	root *Account,
	committedEvents aggregate.CommittedEvents[AccountEvent],
) error {
	for _, event := range committedEvents {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("outbox process: failed to marshal event: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
            INSERT INTO outbox_account_events (aggregate_id, event_name, payload) 
            VALUES (?, ?, ?)
        `, root.ID(), event.EventName(), payload)
		if err != nil {
			return fmt.Errorf("outbox process: insert event: %w", err)
		}
	}
	return nil
}
```

Now let's wire it up in `main`. We create the processor and add it to our `TransactionalRepository`.
```go
func main() {
	db, err := sql.Open("sqlite3", "file:memdb1?mode=memory&cache=shared")
	// ...

	pubsub := examplehelper.NewPubSubMemory[OutboxMessage]()

	outboxProcessor, err := accountv2.NewAccountOutboxProcessor(db)
	// ...

	sqliteLog, err := eventlog.NewSqlite(db)
	// ...

	repo, err := chronicle.NewTransactionalRepository(
		sqliteLog,
		accountMaker,
		nil,
		aggregate.NewProcessorChain(outboxProcessor), // The outbox processor
	)
	// ...
}
```

Next, we need the two background components: a **poller** to read from the outbox and a **subscriber** to listen for published messages. For this example, we'll run them as simple goroutines. 

The **subscriber** is easy—it just listens on a channel and prints what it receives: 
```go
	go func() {
			fmt.Println("\nSubscriber started. Waiting for events...")
			for msg := range subCh {
				fmt.Printf(
					"-> Subscriber received: %s for aggregate %s\n",
					msg.EventName,
					msg.AggregateID,
				)
				wg.Done()
			}
		}()
```

The **poller** is a loop that periodically checks the outbox table. In a single transaction, it reads one message, publishes it, and deletes it. 
```go
go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 1. Begin transaction
				tx, err := db.BeginTx(ctx, nil)
				// ...

				// 2. Select the oldest record
				row := tx.QueryRowContext(ctx, "SELECT id, aggregate_id, event_name, payload FROM outbox_account_events ORDER BY id LIMIT 1")
				// ... scan row

				// 3. Publish the message to the bus
				pubsub.Publish(msg)

				// 4. Delete the record from the outbox
				_, err = tx.ExecContext(ctx, "DELETE FROM outbox_account_events WHERE id = ?", msg.OutboxID)
				// ...

				// 5. Commit the transaction to atomically mark the event as processed.
				if err := tx.Commit(); err != nil {
					fmt.Printf("Error committing outbox transaction: %v", err)
				}
			}
		}
	}()
```

Finally, we save our aggregates as before. This action triggers the entire flow. 

```go
    // Save Alice's and Bob's accounts
    accA, _ := accountv2.Open(...)
    _ = accA.DepositMoney(100)
    _ = accA.DepositMoney(50)
    _, _, err = repo.Save(ctx, accA)

    accB, _ := accountv2.Open(...)
    _ = accB.DepositMoney(200)
    _, _, err = repo.Save(ctx, accB)
```

Running the example shows the whole sequence in action
```
go run examples/7_transactional_outbox/main.go

Saving aggregates... This will write to the outbox table.

Subscriber started. Waiting for events...
Polling goroutine started.

State of 'outbox_account_events' table immediately after save:
┌────┬──────────────────┬─────────────────────────┐
│ ID │   AGGREGATE ID   │       EVENT NAME        │
├────┼──────────────────┼─────────────────────────┤
│ 1  │ alice-account-01 │ account/opened          │
│ 2  │ alice-account-01 │ account/money_deposited │
│ 3  │ alice-account-01 │ account/money_deposited │
│ 4  │ bob-account-02   │ account/opened          │
│ 5  │ bob-account-02   │ account/money_deposited │
└────┴──────────────────┴─────────────────────────┘

Waiting for subscriber to process all events from the pub/sub...
-> Subscriber received: account/opened for aggregate alice-account-01
-> Subscriber received: account/money_deposited for aggregate alice-account-01
-> Subscriber received: account/money_deposited for aggregate alice-account-01
-> Subscriber received: account/opened for aggregate bob-account-02
-> Subscriber received: account/money_deposited for aggregate bob-account-02

All events processed by subscriber.

Outbox table:
┌────┬──────────────┬────────────┐
│ ID │ AGGREGATE ID │ EVENT NAME │
└────┴──────────────┴────────────┘
Polling goroutine stopping.
```

As you can see, the outbox table was populated atomically when the aggregates were saved. The poller then read each entry, published it, and deleted it, resulting in a reliably processed queue and an empty table at the end. 
