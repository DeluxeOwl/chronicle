package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

// SnapshotStrategy defines the policy for when to create a snapshot of an aggregate.
// Implementations of this interface allow for flexible and domain-specific rules
// to decide if a snapshot is wanted after a successful save operation.
//
// Usage:
//
//	// This interface is used when configuring a repository with snapshotting.
//	type myCustomStrategy struct {}
//	func (s *myCustomStrategy) ShouldSnapshot(...) bool {
//		// custom logic
//		return true
//	}
//
//	repoWithSnaps, _ := chronicle.NewEventSourcedRepositoryWithSnapshots(
//	    baseRepo,
//	    snapStore,
//	    snapshotter,
//	    &myCustomStrategy{},
//	)
type SnapshotStrategy[TID ID, E event.Any, R Root[TID, E]] interface {
	ShouldSnapshot(
		// The current context of the Save operation.
		ctx context.Context,
		// The current root
		root R,
		// The version of the aggregate *before* the new events were committed.
		previousVersion version.Version,
		// The new version of the aggregate *after* the events were committed.
		newVersion version.Version,
		// The events that were just committed in this Save operation.
		committedEvents CommittedEvents[E],
	) bool
}

type strategyBuilder[TID ID, E event.Any, R Root[TID, E]] struct{}

// SnapStrategyFor provides a fluent builder for creating common snapshot strategies.
//
// Usage:
//
//	// Create a strategy that snapshots every 10 events.
//	strategy := aggregate.SnapStrategyFor[*account.Account]().EveryNEvents(10)
//
//	// Create a composite strategy.
//	strategy2 := aggregate.SnapStrategyFor[*account.Account]().AnyOf(...)
func SnapStrategyFor[R Root[TID, E], TID ID, E event.Any]() *strategyBuilder[TID, E, R] {
	return &strategyBuilder[TID, E, R]{}
}

type everyNEventsStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	N uint64
}

func (s *everyNEventsStrategy[TID, E, R]) ShouldSnapshot(
	_ context.Context,
	_ R,
	previousVersion, newVersion version.Version,
	_ CommittedEvents[E],
) bool {
	if s.N == 0 {
		return false
	}
	nextSnapshotVersion := (uint64(previousVersion)/s.N + 1) * s.N
	return uint64(newVersion) >= nextSnapshotVersion
}

// EveryNEvents creates a strategy that takes a snapshot every `n` events.
// For example, if n is 100, a snapshot will be taken when the aggregate's
// version crosses 100, 200, 300, and so on.
//
// Usage:
//
//	strategy := aggregate.SnapStrategyFor[*account.Account]().EveryNEvents(100)
func (b *strategyBuilder[TID, E, R]) EveryNEvents(n uint64) *everyNEventsStrategy[TID, E, R] {
	return &everyNEventsStrategy[TID, E, R]{N: n}
}

type onEventsStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	eventsToMatch map[string]struct{}
}

func (s *onEventsStrategy[TID, E, R]) ShouldSnapshot(
	_ context.Context,
	_ R,
	_, _ version.Version,
	committedEvents CommittedEvents[E],
) bool {
	for _, committedEvent := range committedEvents {
		if _, ok := s.eventsToMatch[committedEvent.EventName()]; ok {
			return true
		}
	}

	return false
}

// OnEvents creates a strategy that takes a snapshot only if at least one of the
// specified event types (by name) was part of the committed batch.
//
// Usage:
//
//	// Snapshot when an account is closed or a large withdrawal occurs.
//	strategy := aggregate.SnapStrategyFor[*account.Account]().OnEvents(
//	    "account/closed",
//	    "account/large_withdrawal_made",
//	)
func (b *strategyBuilder[TID, E, R]) OnEvents(eventNames ...string) *onEventsStrategy[TID, E, R] {
	eventsToMatch := make(map[string]struct{}, len(eventNames))
	for _, name := range eventNames {
		eventsToMatch[name] = struct{}{}
	}

	return &onEventsStrategy[TID, E, R]{
		eventsToMatch: eventsToMatch,
	}
}

type customStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	shouldSnapshot func(
		ctx context.Context,
		root R,
		previousVersion version.Version,
		newVersion version.Version,
		committedEvents CommittedEvents[E],
	) bool
}

func (s *customStrategy[TID, E, R]) ShouldSnapshot(
	ctx context.Context,
	root R,
	previousVersion, newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool {
	if s.shouldSnapshot == nil {
		return false
	}

	return s.shouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents)
}

// Custom creates a strategy that gives you complete control by taking a function.
// This function receives the full context of the save operation and returns a boolean.
//
// Usage:
//
//	strategy := aggregate.SnapStrategyFor[*account.Account]().Custom(
//	    func(ctx context.Context, root *account.Account, _, _ version.Version, _ aggregate.CommittedEvents[account.AccountEvent]) bool {
//	        // Only snapshot if the account balance is a multiple of 1000.
//	        return root.Balance() % 1000 == 0
//	    },
//	)
func (b *strategyBuilder[TID, E, R]) Custom(shouldSnapshot func(
	ctx context.Context,
	root R,
	previousVersion version.Version,
	newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool,
) *customStrategy[TID, E, R] {
	return &customStrategy[TID, E, R]{
		shouldSnapshot: shouldSnapshot,
	}
}

type afterCommit[TID ID, E event.Any, R Root[TID, E]] struct{}

func (s *afterCommit[TID, E, R]) ShouldSnapshot(
	_ context.Context,
	_ R,
	_, _ version.Version,
	committedEvents CommittedEvents[E],
) bool {
	return len(committedEvents) > 0
}

// AfterCommit creates a strategy that takes a snapshot after every successful save.
// This is aggressive and should be used with caution, as it can be inefficient.
//
// Usage:
//
//	strategy := aggregate.SnapStrategyFor[*account.Account]().AfterCommit()
func (b *strategyBuilder[TID, E, R]) AfterCommit() *afterCommit[TID, E, R] {
	return &afterCommit[TID, E, R]{}
}

// ...
type anyOf[TID ID, E event.Any, R Root[TID, E]] struct {
	strategies []SnapshotStrategy[TID, E, R]
}

func (s *anyOf[TID, E, R]) ShouldSnapshot(
	ctx context.Context,
	root R,
	previousVersion, newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool {
	for _, snapstrategy := range s.strategies {
		if snapstrategy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
			return true
		}
	}
	return false
}

// AnyOf creates a composite strategy that triggers if *any* of its child strategies would trigger.
// It acts as a logical OR.
//
// Usage:
//
//	// Snapshot every 50 events OR if the account is closed.
//	every50 := aggregate.SnapStrategyFor[*account.Account]().EveryNEvents(50)
//	onClose := aggregate.SnapStrategyFor[*account.Account]().OnEvents("account/closed")
//	strategy := aggregate.SnapStrategyFor[*account.Account]().AnyOf(every50, onClose)
func (b *strategyBuilder[TID, E, R]) AnyOf(
	strategies ...SnapshotStrategy[TID, E, R],
) *anyOf[TID, E, R] {
	return &anyOf[TID, E, R]{
		strategies: strategies,
	}
}

// ....

type allOf[TID ID, E event.Any, R Root[TID, E]] struct {
	strategies []SnapshotStrategy[TID, E, R]
}

func (s *allOf[TID, E, R]) ShouldSnapshot(
	ctx context.Context,
	root R,
	previousVersion, newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool {
	for _, snapstrategy := range s.strategies {
		if !snapstrategy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
			return false
		}
	}
	return true
}

// AllOf creates a composite strategy that triggers only if *all* of its child strategies would trigger.
// It acts as a logical AND.
//
// Usage:
//
//	// A contrived example: snapshot only when a withdrawal happens AND the version is a multiple of 10.
//	onWithdrawal := aggregate.SnapStrategyFor[*account.Account]().OnEvents("account/money_withdrawn")
//	every10 := aggregate.SnapStrategyFor[*account.Account]().EveryNEvents(10)
//	strategy := aggregate.SnapStrategyFor[*account.Account]().AllOf(onWithdrawal, every10)
func (b *strategyBuilder[TID, E, R]) AllOf(
	strategies ...SnapshotStrategy[TID, E, R],
) *allOf[TID, E, R] {
	return &allOf[TID, E, R]{
		strategies: strategies,
	}
}
