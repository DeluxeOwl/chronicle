package memoryadapter

import (
	"context"
	"sync"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/DeluxeOwl/zerrors"
)

type MemoryStoreError string

const (
	ErrAppendEvents MemoryStoreError = "append_events"
)

var _ event.Log = new(MemoryStore)

type MemoryStore struct {
	mu     sync.RWMutex
	events map[event.LogID][]event.RecordedEvent

	// Versions need to be handled per id
	logVersions map[event.LogID]version.Version
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		mu:          sync.RWMutex{},
		events:      map[event.LogID][]event.RecordedEvent{},
		logVersions: make(map[event.LogID]version.Version),
	}
}

func (s *MemoryStore) AppendEvents(ctx context.Context, id event.LogID, expected version.Check, events ...event.Event) (version.Version, error) {
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

	if exp, ok := expected.(version.CheckExact); ok {
		expectedVer := version.Version(exp)
		if currentStreamVersion != expectedVer {
			return 0, zerrors.New(ErrAppendEvents).With("stream", id).Errorf("append events to stream: %w", version.ConflictError{
				Expected: expectedVer,
				Actual:   currentStreamVersion,
			})
		}
	}

	// Store events with versions starting from currentStreamVersion + 1
	recordedEvents := event.ToRecorded(currentStreamVersion, id, events...)
	s.events[id] = append(s.events[id], recordedEvents...)

	// Update and store the new version for this specific stream
	newStreamVersion := currentStreamVersion + version.Version(len(events))
	s.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

// ReadEvents implements event.Store.
func (s *MemoryStore) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) event.RecordedEvents {
	return func(yield func(event.RecordedEvent, error) bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		events, ok := s.events[id]
		if !ok {
			return
		}

		for _, e := range events {
			if e.Version() < selector.From {
				continue
			}

			ctxErr := ctx.Err()

			//nolint:exhaustruct // Not needed.
			if ctxErr != nil && !yield(event.RecordedEvent{}, ctx.Err()) {
				return
			}
			if !yield(e, nil) {
				return
			}
		}
	}
}
