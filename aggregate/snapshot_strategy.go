package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

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
		committedEvents event.CommitedEvents,
	) bool
}

type strategyBuilder[TID ID, E event.Any, R Root[TID, E]] struct{}

func SnapshotStrategyFor[R Root[TID, E], TID ID, E event.Any]() *strategyBuilder[TID, E, R] {
	return &strategyBuilder[TID, E, R]{}
}

type everyNEventsStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	N uint64
}

func (s *everyNEventsStrategy[TID, E, R]) ShouldSnapshot(_ context.Context, _ R, previousVersion, newVersion version.Version, _ event.CommitedEvents) bool {
	if s.N == 0 {
		return false
	}
	nextSnapshotVersion := (uint64(previousVersion)/s.N + 1) * s.N
	return uint64(newVersion) >= nextSnapshotVersion
}

func (b *strategyBuilder[TID, E, R]) EveryNEvents(n uint64) *everyNEventsStrategy[TID, E, R] {
	return &everyNEventsStrategy[TID, E, R]{N: n}
}

type onEventsStrategy[TID ID, E event.Any, R Root[TID, E]] struct {
	eventsToMatch map[string]struct{}
}

func (s *onEventsStrategy[TID, E, R]) ShouldSnapshot(_ context.Context, _ R, _, _ version.Version, committedEvents event.CommitedEvents) bool {
	for _, committedEvent := range committedEvents {
		if _, ok := s.eventsToMatch[committedEvent.EventName()]; ok {
			return true
		}
	}

	return false
}

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
		committedEvents event.CommitedEvents,
	) bool
}

func (s *customStrategy[TID, E, R]) ShouldSnapshot(ctx context.Context, root R, previousVersion, newVersion version.Version, committedEvents event.CommitedEvents) bool {
	if s.shouldSnapshot == nil {
		return false
	}

	return s.shouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents)
}

func (b *strategyBuilder[TID, E, R]) Custom(shouldSnapshot func(
	ctx context.Context,
	root R,
	previousVersion version.Version,
	newVersion version.Version,
	committedEvents event.CommitedEvents,
) bool,
) *customStrategy[TID, E, R] {
	return &customStrategy[TID, E, R]{
		shouldSnapshot: shouldSnapshot,
	}
}

type afterCommit[TID ID, E event.Any, R Root[TID, E]] struct{}

func (s *afterCommit[TID, E, R]) ShouldSnapshot(_ context.Context, _ R, _, _ version.Version, committedEvents event.CommitedEvents) bool {
	return len(committedEvents) > 0
}

func (b *strategyBuilder[TID, E, R]) AfterCommit() *afterCommit[TID, E, R] {
	return &afterCommit[TID, E, R]{}
}

// ...
type anyOf[TID ID, E event.Any, R Root[TID, E]] struct {
	strategies []SnapshotStrategy[TID, E, R]
}

func (s *anyOf[TID, E, R]) ShouldSnapshot(ctx context.Context, root R, previousVersion, newVersion version.Version, committedEvents event.CommitedEvents) bool {
	for _, strategy := range s.strategies {
		if strategy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
			return true
		}
	}
	return false
}

func (b *strategyBuilder[TID, E, R]) AnyOf(strategies ...SnapshotStrategy[TID, E, R]) *anyOf[TID, E, R] {
	return &anyOf[TID, E, R]{
		strategies: strategies,
	}
}

// ....

type allOf[TID ID, E event.Any, R Root[TID, E]] struct {
	strategies []SnapshotStrategy[TID, E, R]
}

func (s *allOf[TID, E, R]) ShouldSnapshot(ctx context.Context, root R, previousVersion, newVersion version.Version, committedEvents event.CommitedEvents) bool {
	for _, strategy := range s.strategies {
		if !strategy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
			return false
		}
	}
	return true
}

func (b *strategyBuilder[TID, E, R]) AllOf(strategies ...SnapshotStrategy[TID, E, R]) *allOf[TID, E, R] {
	return &allOf[TID, E, R]{
		strategies: strategies,
	}
}
