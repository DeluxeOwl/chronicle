package main

import (
	"context"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/sanity-io/litter"
)

func main() {
	// Create a memory event log
	memoryEventLog := eventlog.NewMemory()

	// Create the repository for an account
	accountRepo, err := chronicle.NewEventSourcedRepository(
		memoryEventLog,  // The event log
		NewEmptyAccount, // The constructor for our aggregate
		nil,             // This is an optional parameter called "transformers"
	)
	if err != nil {
		panic(err)
	}

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

	ctx := context.Background()
	version, commitedEvents, err := accountRepo.Save(ctx, account)
	if err != nil {
		panic(err)
	}

	fmt.Printf("version: %d\n", version)
	for _, ev := range commitedEvents {
		litter.Dump(ev)
	}
}
