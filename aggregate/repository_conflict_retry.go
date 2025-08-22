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

// By default, retries up to 3 times on conflict errors.
type ESRepoWithRetry[TID ID, E event.Any, R Root[TID, E]] struct {
	internal Repository[TID, E, R]
	opts     []retry.Option
}

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
