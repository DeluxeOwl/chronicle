package memoryadapter

import (
	"context"
	"fmt"
	"iter"
	"sync"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
)

var _ event.Log = new(MemoryStore)

type MemoryStore struct {
	mu     sync.RWMutex
	events map[event.LogID][]event.StoredEvent

	// Versions need to be handlel per id
	logVersions map[event.LogID]version.Version
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		mu:          sync.RWMutex{},
		events:      map[event.LogID][]event.StoredEvent{},
		logVersions: make(map[event.LogID]version.Version),
	}
}

func (s *MemoryStore) AppendEvents(ctx context.Context, id event.LogID, expected version.Check, events ...event.GenericEvent) (version.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if len(events) == 0 {
		s.mu.RLock()
		currentStreamVersion := s.logVersions[id]
		s.mu.RUnlock()
		return currentStreamVersion, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	currentStreamVersion := s.logVersions[id] // Defaults to 0 if id is not in map

	// Optimistic concurrency check based on the stream's current version
	// TODO: this should be an option, depending if we want or not optimistic concurrency check
	//       A common approach is for CheckAny to bypass the check, and CheckExact to enforce it.
	if exp, ok := expected.(version.CheckExact); ok {
		expectedVer := version.Version(exp)
		if currentStreamVersion != expectedVer {
			return 0, fmt.Errorf("event.MemoryStore: failed to append events to stream '%s', %w", id, version.ConflictError{
				Expected: expectedVer,
				Actual:   currentStreamVersion,
			})
		}
	}

	// Store events with versions starting from currentStreamVersion + 1
	storedEvents := event.GenericEventsToStored(currentStreamVersion, id, events...)
	s.events[id] = append(s.events[id], storedEvents...)

	// Update and store the new version for this specific stream
	newStreamVersion := currentStreamVersion + version.Version(len(events))
	s.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

// ReadEvents implements event.Store.
func (s *MemoryStore) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) iter.Seq[event.StoredEvent] {
	return func(yield func(event.StoredEvent) bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		events, ok := s.events[id]
		if !ok {
			return
		}

		for _, e := range events {
			if e.Version < selector.From {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if !yield(e) {
				return
			}
		}
	}
}
