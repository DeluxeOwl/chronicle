# Optimistic Concurrency & Conflict Errors

You can find this example in [./examples/2_optimistic_concurrency/main.go](../examples/2_optimistic_concurrency/main.go).

What happens if two users try to withdraw money from the same bank account at the exact same time? This is called a "race condition" problem.

Event sourcing handles this using **Optimistic Concurrency Control**. 

`chronicle` handles this for you automatically thanks to the versioning system built into `aggregate.Base` - the struct you embed in your aggregates.

We're gonna use the `Account` example from the `examples/internal/account` package (check the [quickstart](../README.md#quickstart) if you haven't done so).

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

## Further reading

- [Handling conflict errors](handling-conflict-errors.md)
- [Retry with backoff](retry-with-backoff.md)
- [Custom retry](custom-retry.md)
- [How is this different from SQL transactions?](sql-transactions-comparison.md)
- [Will conflicts be a bottleneck?](will-conflicts-be-a-bottleneck.md)
