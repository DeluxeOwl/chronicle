package chronicle

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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

func (store *EventLogMemory) AppendEvents(ctx context.Context, id event.LogID, expected version.Check, events event.RawEvents) (version.Version, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	if len(events) == 0 {
		store.mu.RLock()
		actualLogVersion := store.logVersions[id]
		store.mu.RUnlock()
		return actualLogVersion, nil
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	actualLogVersion := store.logVersions[id] // Defaults to 0 if id is not in map

	if exp, ok := expected.(version.CheckExact); ok {
		expectedVersion := version.Version(exp)
		if actualLogVersion != expectedVersion {
			return 0, fmt.Errorf("append events: %w", version.ConflictError{
				Expected: expectedVersion,
				Actual:   actualLogVersion,
			})
		}
	}

	// Store events with versions starting from actualLogVersion + 1
	eventRecords := events.ToRecords(id, actualLogVersion)

	internal, err := store.recordsToInternal(eventRecords)
	if err != nil {
		return 0, fmt.Errorf("append events: %w", err)
	}

	store.events[id] = append(store.events[id], internal...)

	// Update and store the new version for this specific stream
	newStreamVersion := actualLogVersion + version.Version(len(events))
	store.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

func (store *EventLogMemory) recordsToInternal(records []*event.Record) ([][]byte, error) {
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

func (store *EventLogMemory) memoryRecordToRecord(memoryRecordB []byte) (*event.Record, error) {
	var memoryRecord memoryRecord
	err := json.Unmarshal(memoryRecordB, &memoryRecord)
	if err != nil {
		return nil, fmt.Errorf("unmarshal record to internal: %w", err)
	}

	return event.NewRecord(memoryRecord.Version, memoryRecord.LogID, memoryRecord.EventName, memoryRecord.Data), nil
}

func (store *EventLogMemory) ReadEvents(ctx context.Context, id event.LogID, selector version.Selector) event.Records {
	return func(yield func(*event.Record, error) bool) {
		store.mu.RLock()
		defer store.mu.RUnlock()

		events, ok := store.events[id]
		if !ok {
			return
		}

		for _, internalSerialized := range events {
			record, err := store.memoryRecordToRecord(internalSerialized)

			if err != nil && !yield(nil, err) {
				return
			}

			if record.Version() < selector.From {
				continue
			}

			ctxErr := ctx.Err()

			if ctxErr != nil && !yield(nil, ctx.Err()) {
				return
			}

			if !yield(record, nil) {
				return
			}
		}
	}
}
