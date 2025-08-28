//nolint:cyclop // example.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/accountv2"
	"github.com/DeluxeOwl/chronicle/examples/internal/examplehelper"
	"github.com/DeluxeOwl/chronicle/pkg/timeutils"
	_ "github.com/mattn/go-sqlite3"
)

// A simple struct to represent the message read from the outbox
// and sent over the pub/sub channel.
type OutboxMessage struct {
	OutboxID    int64
	AggregateID string
	EventName   string
	Payload     json.RawMessage
}

//nolint:gocognit,funlen // example.
func main() {
	db, err := sql.Open("sqlite3", "file:memdb1?mode=memory&cache=shared")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	sqlprinter := examplehelper.NewSQLPrinter(db)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pubsub := examplehelper.NewPubSubMemory[OutboxMessage]()

	outboxProcessor, err := accountv2.NewAccountOutboxProcessor(db)
	if err != nil {
		panic(err)
	}

	sqliteLog, err := eventlog.NewSqlite(db)
	if err != nil {
		panic(err)
	}

	timeProvider := timeutils.RealTimeProvider()
	accountMaker := accountv2.NewEmptyMaker(timeProvider)
	repo, err := chronicle.NewTransactionalRepository(
		sqliteLog,
		accountMaker,
		nil,
		aggregate.NewProcessorChain(outboxProcessor), // The outbox processor
	)
	if err != nil {
		panic(err)
	}

	// We expect 5 events total:
	// - Alice: accountOpened, moneyDeposited, moneyDeposited (3)
	// - Bob: accountOpened, moneyDeposited (2)
	const expectedEvents = 5

	var wg sync.WaitGroup
	wg.Add(expectedEvents)

	// Start a subscriber goroutine to listen for published events
	// ⚠️ In a production environment, this would probably be another process (a consumer)
	subCh := pubsub.Subscribe()
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

	// This polling goroutine reads from the outbox table, publishes, and deletes in a loop.
	go func() {
		fmt.Println("Polling goroutine started.")
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				fmt.Println("Polling goroutine stopping.")
				return
			case <-ticker.C:
				// 1. Begin transaction
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					panic(err)
				}

				// 2. Select the oldest record
				row := tx.QueryRowContext(
					ctx,
					"SELECT id, aggregate_id, event_name, payload FROM outbox_account_events ORDER BY id LIMIT 1",
				)

				var msg OutboxMessage
				var payloadBytes []byte
				err = row.Scan(&msg.OutboxID, &msg.AggregateID, &msg.EventName, &payloadBytes)
				if errors.Is(err, sql.ErrNoRows) {
					_ = tx.Rollback()
					continue
				} else if err != nil {
					fmt.Printf("Error scanning outbox row: %v", err)
					_ = tx.Rollback()
					continue
				}

				msg.Payload = payloadBytes

				// 3. Publish the message to the bus
				pubsub.Publish(msg)

				// 4. Delete the record from the outbox
				_, err = tx.ExecContext(
					ctx,
					"DELETE FROM outbox_account_events WHERE id = ?",
					msg.OutboxID,
				)
				if err != nil {
					fmt.Printf("Error deleting from outbox: %v", err)
					_ = tx.Rollback()
					continue
				}

				// 5. Commit the transaction to atomically mark the event as processed.
				if err := tx.Commit(); err != nil {
					fmt.Printf("Error committing outbox transaction: %v", err)
				}
			}
		}
	}()

	fmt.Println("Saving aggregates... This will write to the outbox table.")
	accA, _ := accountv2.Open(accountv2.AccountID("alice-account-01"), timeProvider, "Alice")
	_ = accA.DepositMoney(100)
	_ = accA.DepositMoney(50)
	_, _, err = repo.Save(ctx, accA)
	if err != nil {
		panic(err)
	}

	accB, _ := accountv2.Open(accountv2.AccountID("bob-account-02"), timeProvider, "Bob")
	_ = accB.DepositMoney(200)
	_, _, err = repo.Save(ctx, accB)
	if err != nil {
		panic(err)
	}

	fmt.Println("\nState of 'outbox_account_events' table immediately after save:")
	sqlprinter.Query("SELECT id, aggregate_id, event_name FROM outbox_account_events")

	fmt.Println("\nWaiting for subscriber to process all events from the pub/sub...")
	wg.Wait()
	fmt.Println("\nAll events processed by subscriber.")

	time.Sleep(200 * time.Millisecond)

	fmt.Println("\nOutbox table:")
	sqlprinter.Query("SELECT id, aggregate_id, event_name FROM outbox_account_events")
}
