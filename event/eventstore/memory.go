package eventstore

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/DeluxeOwl/eventuallynow/event"

	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/DeluxeOwl/zerrors"
)

type MemoryError string

const (
	ErrAppendEvents MemoryError = "append_events"
)

var _ event.Log = new(Memory)

type internalRecorded struct {
	Version version.Version `json:"version"`
	LogID   event.LogID     `json:"logID"`

	EventData []byte `json:"eventData" exhaustruct:"optional"`
}

type Memory struct {
	mu sync.RWMutex

	events map[event.LogID][][]byte

	// Versions need to be handled per id
	logVersions map[event.LogID]version.Version
}

func NewMemory() *Memory {
	return &Memory{
		mu:          sync.RWMutex{},
		events:      map[event.LogID][][]byte{},
		logVersions: map[event.LogID]version.Version{},
	}
}

func (s *Memory) AppendEvents(ctx context.Context, id event.LogID, expected version.Check, events ...event.Event) (version.Version, error) {
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
	internal, err := s.recordedToInternal(recordedEvents)
	if err != nil {
		return 0, zerrors.New(ErrAppendEvents).WithError(err)
	}

	s.events[id] = append(s.events[id], internal...)

	// Update and store the new version for this specific stream
	newStreamVersion := currentStreamVersion + version.Version(len(events))
	s.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

func (s *Memory) recordedToInternal(recEvents []*event.RecordedEvent) ([][]byte, error) {
	internalBytes := make([][]byte, len(recEvents))

	for i, r := range recEvents {
		ir := &internalRecorded{
			Version: r.Version(),
			LogID:   r.LogID(),
		}
		b, err := r.EventAny().MarshalEvent()
		if err != nil {
			return nil, fmt.Errorf("serialize event: %w", err)
		}
		ir.EventData = b

		bb, err := json.Marshal(ir)
		if err != nil {
			return nil, fmt.Errorf("marshal internal record: %w", err)
		}
		internalBytes[i] = bb
	}

	return internalBytes, nil
}

func (s *Memory) internalToRecorded(internalMarshaled []byte) (*event.RecordedEvent, error) {
	var ir internalRecorded
	err := json.Unmarshal(internalMarshaled, &ir)
	if err != nil {
		return nil, fmt.Errorf("internal unmarshal: %w", err)
	}

	var e event.EventAny

	ev, err := e.UnmarshalEvent(ir.EventData)
	if err != nil {
		return nil, fmt.Errorf("internal deserialize: %w", err)
	}

	return event.NewRecorded(ir.Version, ir.LogID, ev), nil
}

// ReadEvents implements event.Store.
func (s *Memory) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) event.RecordedEvents {
	return func(yield func(*event.RecordedEvent, error) bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		events, ok := s.events[id]
		if !ok {
			return
		}

		for _, internalSerialized := range events {
			e, err := s.internalToRecorded(internalSerialized)

			if err != nil && !yield(nil, err) {
				return
			}

			if e.Version() < selector.From {
				continue
			}

			ctxErr := ctx.Err()

			if ctxErr != nil && !yield(nil, ctx.Err()) {
				return
			}

			if !yield(e, nil) {
				return
			}
		}
	}
}
