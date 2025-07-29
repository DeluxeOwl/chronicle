package eventlog

import (
	"context"
	"fmt"
	"sync"

	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
)

var _ event.Log = new(Memory)

type Memory struct {
	mu          sync.RWMutex
	events      map[event.LogID][]memStoreRecord
	logVersions map[event.LogID]version.Version
}

type memStoreRecord struct {
	LogID     event.LogID     `json:"logID"`
	Version   version.Version `json:"version"`
	Data      []byte          `json:"data"`
	EventName string          `json:"eventName"`
}

func NewMemory() *Memory {
	return &Memory{
		mu:          sync.RWMutex{},
		events:      map[event.LogID][]memStoreRecord{},
		logVersions: map[event.LogID]version.Version{},
	}
}

func (store *Memory) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	if err := ctx.Err(); err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}

	if len(events) == 0 {
		return version.Zero, fmt.Errorf("append events: %w", ErrNoEvents)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	actualLogVersion := store.logVersions[id] // Defaults to 0 if id is not in map

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, fmt.Errorf("append events: %w", ErrUnsupportedCheck)
	}

	if err := exp.CheckExact(actualLogVersion); err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}

	// Store events with versions starting from actualLogVersion + 1
	eventRecords := events.ToRecords(id, actualLogVersion)

	internal := store.recordsToInternal(eventRecords)

	store.events[id] = append(store.events[id], internal...)

	// Update and store the new version for this specific stream
	newStreamVersion := actualLogVersion + version.Version(len(events))
	store.logVersions[id] = newStreamVersion

	return newStreamVersion, nil
}

func (store *Memory) recordsToInternal(records []*event.Record) []memStoreRecord {
	memoryRecords := make([]memStoreRecord, len(records))

	for i, record := range records {
		memoryRecord := memStoreRecord{
			LogID:     record.LogID(),
			Version:   record.Version(),
			Data:      record.Data(),
			EventName: record.EventName(),
		}

		memoryRecords[i] = memoryRecord
	}

	return memoryRecords
}

func (store *Memory) memoryRecordToRecord(memRecord *memStoreRecord) *event.Record {
	return event.NewRecord(memRecord.Version, memRecord.LogID, memRecord.EventName, memRecord.Data)
}

func (store *Memory) ReadEvents(
	ctx context.Context,
	id event.LogID,
	selector version.Selector,
) event.Records {
	return func(yield func(*event.Record, error) bool) {
		store.mu.RLock()
		defer store.mu.RUnlock()

		events, ok := store.events[id]
		if !ok {
			return
		}

		for _, internalSerialized := range events {
			record := store.memoryRecordToRecord(&internalSerialized)

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
