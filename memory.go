package chronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/convert"
	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
)

var _ event.Log = new(EventLogMemory)

type EventLogMemory struct {
	mu          sync.RWMutex
	events      map[event.LogID][][]byte
	logVersions map[event.LogID]version.Version
}

type memoryRecord struct {
	LogID     event.LogID     `json:"logID"`
	Version   version.Version `json:"version"`
	Data      []byte          `json:"data"`
	EventName string          `json:"eventName"`
}

func NewEventLogMemory() *EventLogMemory {
	return &EventLogMemory{
		mu:          sync.RWMutex{},
		events:      map[event.LogID][][]byte{},
		logVersions: map[event.LogID]version.Version{},
	}
}

func (s *EventLogMemory) AppendEvents(ctx context.Context, id event.LogID, expected version.Check, events []event.Raw) (version.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if len(events) == 0 {
		s.mu.RLock()
		currentLogVersion := s.logVersions[id]
		s.mu.RUnlock()
		return currentLogVersion, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	actualLogVersion := s.logVersions[id] // Defaults to 0 if id is not in map

	if exp, ok := expected.(version.CheckExact); ok {
		expectedVersion := version.Version(exp)
		if actualLogVersion != expectedVersion {
			return 0, fmt.Errorf("append events: %w", version.ConflictError{
				Expected: expectedVersion,
				Actual:   actualLogVersion,
			})
		}
	}

	// Store events with versions starting from currentStreamVersion + 1
	recordedEvents := convert.RawToRecorded(actualLogVersion, id, events)

	internal, err := s.recordsToInternal(recordedEvents)
	if err != nil {
		return 0, fmt.Errorf("append events: %w", err)
	}

	s.events[id] = append(s.events[id], internal...)

	// Update and store the new version for this specific stream
	newStreamVersion := actualLogVersion + version.Version(len(events))
	s.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

func (s *EventLogMemory) recordsToInternal(records []*event.Record) ([][]byte, error) {
	memoryRecords := make([][]byte, len(records))

	for i, record := range records {
		memoryRecord := memoryRecord{
			LogID:     record.LogID(),
			Version:   record.Version(),
			Data:      record.Data(),
			EventName: record.EventName(),
		}

		memoryRecordB, err := json.Marshal(memoryRecord)
		if err != nil {
			return nil, fmt.Errorf("marshal records to internal: %w", err)
		}

		memoryRecords[i] = memoryRecordB
	}

	return memoryRecords, nil
}

func (s *EventLogMemory) unmarshalInternalToRecords(internalMarshaled []byte) (*event.Record, error) {
	var memoryRecord memoryRecord
	err := json.Unmarshal(internalMarshaled, &memoryRecord)
	if err != nil {
		return nil, fmt.Errorf("unmarshal record to internal: %w", err)
	}

	return event.NewRecord(memoryRecord.Version, memoryRecord.LogID, memoryRecord.EventName, memoryRecord.Data), nil
}

func (s *EventLogMemory) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) event.Records {
	return func(yield func(*event.Record, error) bool) {
		s.mu.RLock()
		defer s.mu.RUnlock()

		events, ok := s.events[id]
		if !ok {
			return
		}

		for _, internalSerialized := range events {
			e, err := s.unmarshalInternalToRecords(internalSerialized)

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
