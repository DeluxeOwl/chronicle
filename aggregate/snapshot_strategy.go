package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

// SnapshotPolicy defines the policy for when to create a snapshot of an aggregate.
// Implementations of this interface allow for flexible and domain-specific rules
// to decide if a snapshot is wanted after a successful save operation.
//
// Usage:
//
//	// This interface is used when configuring a repository with snapshotting.
//	type myCustomPolicy struct {}
//	func (s *myCustomPolicy) ShouldSnapshot(...) bool {
//		// custom logic
//		return true
//	}
//
//	repoWithSnaps, _ := chronicle.NewEventSourcedRepositoryWithSnapshots(
//	    baseRepo,
//	    snapStore,
//	    snapshotter,
//	    &myCustomPolicy{},
//	)
type SnapshotPolicy[TID ID, E event.Any, R Root[TID, E]] interface {
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

type policyBuilder[TID ID, E event.Any, R Root[TID, E]] struct{}

// SnapPolicyFor provides a fluent builder for creating common snapshot policies.
//
// Usage:
//
//	// Create a policy that snapshots every 10 events.
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().EveryNEvents(10)
//
//	// Create a composite policy.
//	policy2 := aggregate.SnapPolicyFor[*account.Account]().AnyOf(...)
func SnapPolicyFor[R Root[TID, E], TID ID, E event.Any]() *policyBuilder[TID, E, R] {
	return &policyBuilder[TID, E, R]{}
}

type everyNEventsPolicy[TID ID, E event.Any, R Root[TID, E]] struct {
	N uint64
}

func (s *everyNEventsPolicy[TID, E, R]) ShouldSnapshot(
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

// EveryNEvents creates a policy that takes a snapshot every `n` events.
// For example, if n is 100, a snapshot will be taken when the aggregate's
// version crosses 100, 200, 300, and so on.
//
// Usage:
//
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().EveryNEvents(100)
func (b *policyBuilder[TID, E, R]) EveryNEvents(n uint64) *everyNEventsPolicy[TID, E, R] {
	return &everyNEventsPolicy[TID, E, R]{N: n}
}

type onEventsPolicy[TID ID, E event.Any, R Root[TID, E]] struct {
	eventsToMatch map[string]struct{}
}

func (s *onEventsPolicy[TID, E, R]) ShouldSnapshot(
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

// OnEvents creates a policy that takes a snapshot only if at least one of the
// specified event types (by name) was part of the committed batch.
//
// Usage:
//
//	// Snapshot when an account is closed or a large withdrawal occurs.
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().OnEvents(
//	    "account/closed",
//	    "account/large_withdrawal_made",
//	)
func (b *policyBuilder[TID, E, R]) OnEvents(eventNames ...string) *onEventsPolicy[TID, E, R] {
	eventsToMatch := make(map[string]struct{}, len(eventNames))
	for _, name := range eventNames {
		eventsToMatch[name] = struct{}{}
	}

	return &onEventsPolicy[TID, E, R]{
		eventsToMatch: eventsToMatch,
	}
}

type customPolicy[TID ID, E event.Any, R Root[TID, E]] struct {
	shouldSnapshot func(
		ctx context.Context,
		root R,
		previousVersion version.Version,
		newVersion version.Version,
		committedEvents CommittedEvents[E],
	) bool
}

func (s *customPolicy[TID, E, R]) ShouldSnapshot(
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

// Custom creates a policy that gives you complete control by taking a function.
// This function receives the full context of the save operation and returns a boolean.
//
// Usage:
//
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().Custom(
//	    func(ctx context.Context, root *account.Account, _, _ version.Version, _ aggregate.CommittedEvents[account.AccountEvent]) bool {
//	        // Only snapshot if the account balance is a multiple of 1000.
//	        return root.Balance() % 1000 == 0
//	    },
//	)
func (b *policyBuilder[TID, E, R]) Custom(shouldSnapshot func(
	ctx context.Context,
	root R,
	previousVersion version.Version,
	newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool,
) *customPolicy[TID, E, R] {
	return &customPolicy[TID, E, R]{
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

// AfterCommit creates a policy that takes a snapshot after every successful save.
// This is aggressive and should be used with caution, as it can be inefficient.
//
// Usage:
//
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().AfterCommit()
func (b *policyBuilder[TID, E, R]) AfterCommit() *afterCommit[TID, E, R] {
	return &afterCommit[TID, E, R]{}
}

// ...
type anyOf[TID ID, E event.Any, R Root[TID, E]] struct {
	policies []SnapshotPolicy[TID, E, R]
}

func (s *anyOf[TID, E, R]) ShouldSnapshot(
	ctx context.Context,
	root R,
	previousVersion, newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool {
	for _, snapPolicy := range s.policies {
		if snapPolicy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
			return true
		}
	}
	return false
}

// AnyOf creates a composite policy that triggers if *any* of its child policies would trigger.
// It acts as a logical OR.
//
// Usage:
//
//	// Snapshot every 50 events OR if the account is closed.
//	every50 := aggregate.SnapPolicyFor[*account.Account]().EveryNEvents(50)
//	onClose := aggregate.SnapPolicyFor[*account.Account]().OnEvents("account/closed")
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().AnyOf(every50, onClose)
func (b *policyBuilder[TID, E, R]) AnyOf(
	policies ...SnapshotPolicy[TID, E, R],
) *anyOf[TID, E, R] {
	return &anyOf[TID, E, R]{
		policies: policies,
	}
}

// ....

type allOf[TID ID, E event.Any, R Root[TID, E]] struct {
	policies []SnapshotPolicy[TID, E, R]
}

func (s *allOf[TID, E, R]) ShouldSnapshot(
	ctx context.Context,
	root R,
	previousVersion, newVersion version.Version,
	committedEvents CommittedEvents[E],
) bool {
	for _, snapPolicy := range s.policies {
		if !snapPolicy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
			return false
		}
	}
	return true
}

// AllOf creates a composite policy that triggers only if *all* of its child policies would trigger.
// It acts as a logical AND.
//
// Usage:
//
//	// A contrived example: snapshot only when a withdrawal happens AND the version is a multiple of 10.
//	onWithdrawal := aggregate.SnapPolicyFor[*account.Account]().OnEvents("account/money_withdrawn")
//	every10 := aggregate.SnapPolicyFor[*account.Account]().EveryNEvents(10)
//	policy :=  aggregate.SnapPolicyFor[*account.Account]().AllOf(onWithdrawal, every10)
func (b *policyBuilder[TID, E, R]) AllOf(
	policies ...SnapshotPolicy[TID, E, R],
) *allOf[TID, E, R] {
	return &allOf[TID, E, R]{
		policies: policies,
	}
}
