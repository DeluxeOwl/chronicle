# Retry with backoff

The retry cycle can be handled in a loop. If the conflicts are frequent, you might add a backoff.

You can wrap the repository with `chronicle.NewEventSourcedRepositoryWithRetry`, which uses github.com/avast/retry-go/v4 for retries. 

**The default is to retry 3 times on conflict errors**. You can customize the retry mechanism by providing `retry.Option(s)`.

```go
	import "github.com/avast/retry-go/v4"

	ar, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	accountRepo := chronicle.NewEventSourcedRepositoryWithRetry(ar)
	
	accountRepo := chronicle.NewEventSourcedRepositoryWithRetry(ar, retry.Attempts(5),
		retry.Delay(100*time.Millisecond),    // Initial delay
		retry.MaxDelay(10*time.Second),       // Cap the maximum delay
		retry.DelayType(retry.BackOffDelay),  // Exponential backoff
		retry.MaxJitter(50*time.Millisecond), // Add randomness
	)
```
