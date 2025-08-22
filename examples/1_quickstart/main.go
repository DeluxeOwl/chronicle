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

	// Create the repository for an account
	accountRepo, err := chronicle.NewEventSourcedRepository(
		memoryEventLog,   // The event log
		account.NewEmpty, // The constructor for our aggregate
		nil,              // This is an optional parameter called "transformers"
	)
	if err != nil {
		panic(err)
	}

	// Create an acc
	acc, err := account.Open(account.AccountID("123"), time.Now())
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

	ctx := context.Background()
	version, committedEvents, err := accountRepo.Save(ctx, acc)
	if err != nil {
		panic(err)
	}

	fmt.Printf("version: %d\n", version)
	for _, ev := range committedEvents {
		litter.Dump(ev)
	}
}
