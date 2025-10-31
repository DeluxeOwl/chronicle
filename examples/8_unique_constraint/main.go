package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/accountv2"
	"github.com/DeluxeOwl/chronicle/examples/internal/examplehelper"
	"github.com/DeluxeOwl/chronicle/pkg/timeutils"
	_ "github.com/mattn/go-sqlite3"
)

var _ event.SyncProjection[*sql.Tx] = &uniqueUsernameProjection{}

type uniqueUsernameProjection struct{}

type HolderNameData struct {
	HolderName string `json:"holderName"`
}

func (u *uniqueUsernameProjection) Handle(
	ctx context.Context,
	tx *sql.Tx,
	records []*event.Record,
) error {
	stmt, err := tx.PrepareContext(ctx, "INSERT INTO unique_usernames (username) VALUES (?)")
	if err != nil {
		return fmt.Errorf("failed to prepare statement for unique username projection: %w", err)
	}
	defer stmt.Close()

	for _, record := range records {
		//nolint:exhaustruct // not needed.
		holderName := HolderNameData{}

		if err := json.Unmarshal(record.Data(), &holderName); err != nil {
			return fmt.Errorf("unmarshal HolderNameData: %w", err)
		}

		// Attempt to insert the username. If it violates the UNIQUE constraint,
		// this will return an error, which will cause the entire transaction to fail.
		_, err := stmt.ExecContext(ctx, holderName.HolderName)
		if err != nil {
			// This is the expected error for a duplicate username
			return fmt.Errorf("username '%s' already exists: %w", holderName.HolderName, err)
		}
	}

	return nil
}

func (u *uniqueUsernameProjection) MatchesEvent(eventName string) bool {
	return eventName == accountv2.EventNameAccountOpened
}

func main() {
	db, err := sql.Open("sqlite3", "file:memdb1?mode=memory&cache=shared")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	_, err = db.Exec(`
        CREATE TABLE IF NOT EXISTS unique_usernames (
			username TEXT NOT NULL UNIQUE
		);
    `)
	if err != nil {
		panic(err)
	}

	sqlprinter := examplehelper.NewSQLPrinter(db)

	initialSqliteLog, err := eventlog.NewSqlite(db)
	if err != nil {
		panic(err)
	}
	sqliteLog := event.NewLogWithProjection(initialSqliteLog, &uniqueUsernameProjection{})

	timeProvider := timeutils.RealTimeProvider()
	accountMaker := accountv2.NewEmptyMaker(timeProvider)

	accountRepo, err := chronicle.NewTransactionalRepository(
		sqliteLog,
		accountMaker,
		nil,
		nil,
	)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	// Alice's account
	accA, _ := accountv2.Open(accountv2.AccountID("alice-account-01"), timeProvider, "Alice")
	_ = accA.DepositMoney(100)
	_ = accA.DepositMoney(50)
	_, _, err = accountRepo.Save(ctx, accA)
	if err != nil {
		panic(err)
	}

	fmt.Println("\nState of unique usernames table:")
	sqlprinter.Query("SELECT username FROM unique_usernames")

	fmt.Println("\nAttempting to create a duplicate user 'Alice'")
	accC, _ := accountv2.Open(accountv2.AccountID("duplicate-alice-03"), timeProvider, "Alice")
	_, _, err = accountRepo.Save(ctx, accC)
	if err != nil {
		fmt.Printf("Successfully prevented duplicate user. Error: %v\n", err)
	} else {
		fmt.Println("ERROR: Duplicate user was created, which should not happen.")
	}

	fmt.Println("\nFinal state of unique usernames table:")
	sqlprinter.Query("SELECT username FROM unique_usernames")

	fmt.Println("\nAll events (note that the duplicate 'Alice' event was not saved):")
	sqlprinter.Query(
		"SELECT log_id, version, event_name, json_extract(data, '$') as data FROM chronicle_events",
	)
}
