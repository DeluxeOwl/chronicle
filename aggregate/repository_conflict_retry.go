package aggregate

import (
	"context"
	"errors"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
	"github.com/avast/retry-go/v4"
)

var _ Repository[testAggID, testAggEvent, *testAgg] = (*ESRepoWithRetry[testAggID, testAggEvent, *testAgg])(
	nil,
)

// ESRepoWithRetry is a repository decorator that provides automatic retry logic
// for the Save method, specifically for handling optimistic concurrency conflicts.
//
// By default, it retries a failing Save operation up to 3 times if the
// error is a `version.ConflictError`. This behavior can be customized
// by providing `retry.Option` values.
//
// Usage:
//
//	baseRepo, err := chronicle.NewEventSourcedRepository(
//	    eventLog,
//	    account.NewEmpty,
//	    nil,
//	)
//	if err != nil { /* ... */ }
//
//	// Wrap the base repository to add retry capabilities.
//	repoWithRetry := chronicle.NewESRepoWithRetry(
//	    baseRepo,
//	    retry.Attempts(5),
//	    retry.Delay(100 * time.Millisecond),
//	    retry.DelayType(retry.BackOffDelay),
//	)
//
//	// Now, calls to repoWithRetry.Save(ctx, agg) will be retried on conflict.
type ESRepoWithRetry[TID ID, E event.Any, R Root[TID, E]] struct {
	internal Repository[TID, E, R]
	opts     []retry.Option
}

// NewESRepoWithRetry wraps an existing repository with retry logic.
//
// It takes the repository to be wrapped and an optional, variadic list of
// `retry.Option`s from `github.com/avast/retry-go/v4` to customize the
// retry strategy (e.g., number of attempts, delay, backoff). If no options
// are provided, it defaults to 3 attempts on conflict errors.
//
// Returns a new `ESRepoWithRetry` instance.
func NewESRepoWithRetry[TID ID, E event.Any, R Root[TID, E]](
	repo Repository[TID, E, R],
	opts ...retry.Option,
) *ESRepoWithRetry[TID, E, R] {
	return &ESRepoWithRetry[TID, E, R]{
		internal: repo,
		opts:     opts,
	}
}

func (e *ESRepoWithRetry[TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return e.internal.Get(ctx, id)
}

func (e *ESRepoWithRetry[TID, E, R]) GetVersion(
	ctx context.Context,
	id TID,
	selector version.Selector,
) (R, error) {
	return e.internal.GetVersion(ctx, id, selector)
}

func (e *ESRepoWithRetry[TID, E, R]) LoadAggregate(
	ctx context.Context,
	root R,
	id TID,
	selector version.Selector,
) error {
	return e.internal.LoadAggregate(ctx, root, id, selector)
}

// Save attempts to persist the aggregate's uncommitted events. If the underlying
// repository's Save method returns a `version.ConflictError`, this method
// will automatically retry the operation according to its configured policy.
//
// Returns the new version of the aggregate and the committed events upon a
// successful save. If all retries fail, it returns the last error encountered.
func (e *ESRepoWithRetry[TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommittedEvents[E], error) {
	type saveResult struct {
		version         version.Version
		committedEvents CommittedEvents[E]
	}

	opts := append([]retry.Option{
		retry.Attempts(3),
		retry.Context(ctx),
		retry.RetryIf(func(err error) bool {
			var conflictErr *version.ConflictError
			return errors.As(err, &conflictErr)
		}),
	}, e.opts...)

	result, err := retry.DoWithData(
		func() (saveResult, error) {
			version, committedEvents, err := e.internal.Save(ctx, root)
			if err != nil {
				return saveResult{}, err
			}
			return saveResult{
				version:         version,
				committedEvents: committedEvents,
			}, nil
		},
		opts...,
	)
	if err != nil {
		var zero version.Version
		var zeroCE CommittedEvents[E]
		return zero, zeroCE, err
	}

	return result.version, result.committedEvents, nil
}
