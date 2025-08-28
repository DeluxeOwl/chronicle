package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/accountv2"
	"github.com/DeluxeOwl/chronicle/examples/internal/examplehelper"
	"github.com/DeluxeOwl/chronicle/pkg/timeutils"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	sqlprinter := examplehelper.NewSQLPrinter(db)

	sqliteLog, err := eventlog.NewSqlite(db)
	if err != nil {
		panic(err)
	}

	timeProvider := timeutils.RealTimeProvider()
	accountMaker := accountv2.NewEmptyMaker(timeProvider)

	accountProcessor, err := accountv2.NewAccountsWithNameProcessor(db)
	if err != nil {
		panic(err)
	}

	accountRepo, err := chronicle.NewTransactionalRepository(
		sqliteLog,
		accountMaker,
		nil,
		aggregate.NewProcessorChain(
			accountProcessor,
		), // Our transactional processor.
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

	// Bob's account
	accB, _ := accountv2.Open(accountv2.AccountID("bob-account-02"), timeProvider, "Bob")
	_ = accB.DepositMoney(200)
	_, _, err = accountRepo.Save(ctx, accB)
	if err != nil {
		panic(err)
	}
	sqlprinter.Query("SELECT account_id, holder_name FROM projection_accounts")

	fmt.Println("All events:")
	sqlprinter.Query(
		"SELECT global_version, log_id, version, event_name, json_extract(data, '$') as data FROM chronicle_events",
	)
}
