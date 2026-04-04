# Handling conflict errors

How do you handle the error above? The most common way is to **retry the command**:
1. Re-load the aggregate from the repository to get the absolute latest state and version
2. Re-run the original command
3. Try to Save again

```go
	// 💥 Oh no! A conflict error occurred!
	//...
	//... try to withdraw again
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
	fmt.Printf("User A saved successfully! Version is %d and balance $%d\n", version, accUserA.Balance())
	// User A saved successfully! Version is 4 and balance $50
```
