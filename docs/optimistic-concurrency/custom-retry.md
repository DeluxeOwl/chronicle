# Custom retry

Example of wrapping a repository with a custom `Save` method with retries:

```go
type SaveResult struct {
	Version         version.Version
	CommittedEvents aggregate.CommittedEvents[account.AccountEvent]
}

type SaverWithRetry struct {
	saver aggregate.Saver[account.AccountID, account.AccountEvent, *account.Account]
}

func (s *SaverWithRetry) Save(ctx context.Context, root *account.Account) (version.Version, aggregate.CommittedEvents[account.AccountEvent], error) {
	result, err := retry.DoWithData(
		func() (SaveResult, error) {
			version, committedEvents, err := s.saver.Save(ctx, root)
			if err != nil {
				return SaveResult{}, err
			}
			return SaveResult{
				Version:         version,
				CommittedEvents: committedEvents,
			}, nil
		},
		retry.Attempts(3),
		retry.Context(ctx),
		retry.RetryIf(func(err error) bool {
			// Only retry on ConflictErr or specific errors
			var conflictErr *version.ConflictError
			return errors.As(err, &conflictErr)
		}),
	)
	if err != nil {
		var zero version.Version
		var zeroCE aggregate.CommittedEvents[account.AccountEvent]
		return zero, zeroCE, err
	}

	return result.Version, result.CommittedEvents, nil
}
// ...
	accountRepo, _ := chronicle.NewEventSourcedRepository(
		memoryEventLog,
		account.NewEmpty,
		nil,
	)

	repoWithRetry := &aggregate.FusedRepo[account.AccountID, account.AccountEvent, *account.Account]{
		AggregateLoader: accountRepo,
		VersionedGetter: accountRepo,
		Getter:          accountRepo,
		Saver: &SaverWithRetry{
			saver: accountRepo,
		},
	}
```
