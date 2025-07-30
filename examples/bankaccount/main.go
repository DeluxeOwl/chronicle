package main

import (
	"context"

	"github.com/DeluxeOwl/chronicle"
)

func main() {
	ctx := context.Background()

	memlog := chronicle.NewEventLogMemory()
	accountRepo, err := chronicle.NewEventSourcedRepository(memlog, NewEmptyAccount)
	if err != nil {
		panic(err)
	}

	// Open account-1 with $100
	acc1, err := OpenAccount("account-1", 100)
	if err != nil {
		panic(err)
	}

	_, _, err = accountRepo.Save(ctx, acc1)
	if err != nil {
		panic(err)
	}

	// Open account-1 with $230
	acc2, err := OpenAccount("account-2", 230)
	if err != nil {
		panic(err)
	}

	_, _, err = accountRepo.Save(ctx, acc2)
	if err != nil {
		panic(err)
	}
}
