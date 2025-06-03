package eventstore

import (
	"context"
	"encoding/json"
	"errors"
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

type Memory struct {
	mu sync.RWMutex

	registry event.Registry
	events   map[event.LogID][][]byte

	// Versions need to be handled per id
	logVersions map[event.LogID]version.Version
}

type internalRecord struct {
	event.RecordedEventSerializableFields
	Data []byte `json:"data"`
}

type OptionMemory func(*Memory)

func WithRegistryMemory(registry event.Registry) OptionMemory {
	return func(m *Memory) {
		m.registry = registry
	}
}

func NewMemory(opts ...OptionMemory) *Memory {
	m := &Memory{
		registry:    event.GlobalRegistry,
		mu:          sync.RWMutex{},
		events:      map[event.LogID][][]byte{},
		logVersions: map[event.LogID]version.Version{},
	}

	for _, o := range opts {
		o(m)
	}

	return m
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

	internal, err := s.marshalRecordedToInternal(recordedEvents)
	if err != nil {
		return 0, zerrors.New(ErrAppendEvents).WithError(err)
	}

	s.events[id] = append(s.events[id], internal...)

	// Update and store the new version for this specific stream
	newStreamVersion := currentStreamVersion + version.Version(len(events))
	s.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

func (s *Memory) marshalRecordedToInternal(recEvents []*event.RecordedEvent) ([][]byte, error) {
	internalBytes := make([][]byte, len(recEvents))

	for i, r := range recEvents {
		rbytes, err := event.Marshal(r.EventAny())
		if err != nil {
			return nil, fmt.Errorf("marshal snap record: %w", err)
		}

		ir := internalRecord{
			RecordedEventSerializableFields: r.SerializableFields(),
			Data:                            rbytes,
		}

		rbytes, err = json.Marshal(ir)
		if err != nil {
			return nil, fmt.Errorf("marshal internal record: %w", err)
		}

		internalBytes[i] = rbytes
	}

	return internalBytes, nil
}

func (s *Memory) unmarshalInternalToRecorded(internalMarshaled []byte) (*event.RecordedEvent, error) {
	var ir internalRecord
	err := json.Unmarshal(internalMarshaled, &ir)
	if err != nil {
		return nil, fmt.Errorf("internal unmarshal record: %w", err)
	}

	fact, ok := s.registry.NewEvent(ir.EventName)
	if !ok {
		// TODO: errors
		return nil, errors.New("factory not registered for " + ir.EventName)
	}

	ev := fact()
	err = event.Unmarshal(ir.Data, ev)
	if err != nil {
		return nil, fmt.Errorf("internal unmarshal record data: %w", err)
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
			e, err := s.unmarshalInternalToRecorded(internalSerialized)

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
