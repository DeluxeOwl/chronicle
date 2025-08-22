package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/examples/internal/account"
	"github.com/DeluxeOwl/chronicle/version"
	// "github.com/avast/retry-go/v4"
)

func main() {
	memoryEventLog := eventlog.NewMemory()
	accountRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	// You can wrap the repo
	//
	// accountRepo := chronicle.NewEventSourcedRepositoryWithRetry(ar)

	// More robust retrying
	//
	// accountRepo := chronicle.NewEventSourcedRepositoryWithRetry(ar, retry.Attempts(5),
	// 	retry.Delay(100*time.Millisecond),    // Initial delay
	// 	retry.MaxDelay(10*time.Second),       // Cap the maximum delay
	// 	retry.DelayType(retry.BackOffDelay),  // Exponential backoff
	// 	retry.MaxJitter(50*time.Millisecond), // Add randomness
	// )

	ctx := context.Background()
	accID := account.AccountID("acc-123")

	acc, _ := account.Open(accID, time.Now())
	_ = acc.DepositMoney(200) // balance: 200

	// The account starts at version 0, `Open` is event 1, `Deposit` is event 2.
	// After saving, the version will be 2.
	_, _, _ = accountRepo.Save(ctx, acc)
	fmt.Printf("Initial account saved. Balance: 200, Version: %d\n\n", acc.Version())

	accUserA, _ := accountRepo.Get(ctx, accID)
	fmt.Printf(
		"User A loads account. Version: %d, Balance: %d\n",
		accUserA.Version(),
		accUserA.Balance(),
	)

	accUserB, _ := accountRepo.Get(ctx, accID)
	fmt.Printf(
		"User B loads account. Version: %d, Balance: %d\n\n",
		accUserB.Version(),
		accUserB.Balance(),
	)

	_, _ = accUserB.WithdrawMoney(50)
	_, _, _ = accountRepo.Save(ctx, accUserB)
	fmt.Printf("User B withdraws $50 and saves. Account is now at version %d\n", accUserB.Version())

	// Now, User A tries to withdraw $100. The business logic check passes
	// because their copy of the account *thinks* the balance is still $200.
	_, _ = accUserA.WithdrawMoney(100)
	fmt.Println("User A tries to withdraw $100 and save...")

	// But the Save fails! The repository expected version 2, but the DB has version 3.
	_, _, err := accountRepo.Save(ctx, accUserA)
	if err != nil {
		var conflictErr *version.ConflictError
		if errors.As(err, &conflictErr) {
			fmt.Println("\n💥 Oh no! A conflict error occurred!")
			fmt.Printf(
				"   User A's save failed because it expected version %d, but the actual version was %d.\n",
				conflictErr.Expected,
				conflictErr.Actual,
			)
		} else {
			// Other save errors
			panic(err)
		}
	}

	// Conflict handling
	//
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
	fmt.Printf(
		"User A saved successfully! Version is %d and balance $%d\n",
		version,
		accUserA.Balance(),
	)
}
