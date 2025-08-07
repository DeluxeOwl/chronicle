package eventlog

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/DeluxeOwl/chronicle/event"

	"github.com/DeluxeOwl/chronicle/version"
)

var (
	_ event.GlobalLog                    = new(Memory)
	_ event.Log                          = new(Memory)
	_ event.TransactionalEventLog[MemTx] = new(Memory)
)

type Memory struct {
	events        map[event.LogID][]memStoreRecord
	logVersions   map[event.LogID]version.Version
	globalVersion version.Version
	mu            sync.RWMutex
}

type memStoreRecord struct {
	LogID         event.LogID     `json:"logID"`
	EventName     string          `json:"eventName"`
	Data          []byte          `json:"data"`
	Version       version.Version `json:"version"`
	GlobalVersion version.Version `json:"globalVersion"`
}

func NewMemory() *Memory {
	return &Memory{
		mu:            sync.RWMutex{},
		events:        map[event.LogID][]memStoreRecord{},
		logVersions:   map[event.LogID]version.Version{},
		globalVersion: version.Zero,
	}
}

// MemTx is a dummy transaction handle for the in-memory store.
// Its presence in a function signature indicates that the function
// must be called within the critical section managed by WithinTx.
type MemTx struct{}

func (mem *Memory) AppendEvents(
	ctx context.Context,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, error) {
	var newVersion version.Version

	err := mem.WithinTx(ctx, func(ctx context.Context, tx MemTx) error {
		v, _, err := mem.AppendInTx(ctx, tx, id, expected, events)
		if err != nil {
			return err
		}
		newVersion = v
		return nil
	})
	if err != nil {
		return version.Zero, fmt.Errorf("append events: %w", err)
	}

	return newVersion, nil
}

func (mem *Memory) AppendInTx(
	ctx context.Context,
	_ MemTx,
	id event.LogID,
	expected version.Check,
	events event.RawEvents,
) (version.Version, []*event.Record, error) {
	if err := ctx.Err(); err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", err)
	}

	if len(events) == 0 {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrNoEvents)
	}

	// The lock is already held by WithinTx.
	actualLogVersion := mem.logVersions[id]

	exp, ok := expected.(version.CheckExact)
	if !ok {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", ErrUnsupportedCheck)
	}

	if err := exp.CheckExact(actualLogVersion); err != nil {
		return version.Zero, nil, fmt.Errorf("append in tx: %w", err)
	}

	records := events.ToRecords(id, actualLogVersion)
	internal := mem.recordsToInternal(records, mem.globalVersion)
	mem.events[id] = append(mem.events[id], internal...)

	newStreamVersion := actualLogVersion + version.Version(len(events))
	mem.logVersions[id] = newStreamVersion
	mem.globalVersion += version.Version(len(events))

	return newStreamVersion, records, nil
}

// WithinTx executes the given function within a mutex-protected critical section,
// simulating a transaction.
// Note: This simple implementation does not support rollback on error; changes made
// to the store before an error occurs within the function will persist.
func (mem *Memory) WithinTx(
	ctx context.Context,
	fn func(ctx context.Context, tx MemTx) error,
) error {
	mem.mu.Lock()
	defer mem.mu.Unlock()

	return fn(ctx, MemTx{})
}

func (store *Memory) recordsToInternal(
	records []*event.Record,
	startingGlobalVersion version.Version,
) []memStoreRecord {
	memoryRecords := make([]memStoreRecord, len(records))

	for i, record := range records {
		memoryRecord := memStoreRecord{
			LogID:   record.LogID(),
			Version: record.Version(),
			//nolint:gosec // not a problem.
			GlobalVersion: startingGlobalVersion + version.Version(i) + 1,
			Data:          record.Data(),
			EventName:     record.EventName(),
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

func (mem *Memory) ReadAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		mem.mu.RLock()
		allEvents := make([]memStoreRecord, 0)
		for _, logEvents := range mem.events {
			allEvents = append(allEvents, logEvents...)
		}
		mem.mu.RUnlock()

		sort.Slice(allEvents, func(i, j int) bool {
			return allEvents[i].GlobalVersion < allEvents[j].GlobalVersion
		})

		for _, memRecord := range allEvents {
			if memRecord.GlobalVersion < globalSelector.From {
				continue
			}

			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}

			// When reading all events, the record's version is the global version.
			record := event.NewGlobalRecord(
				memRecord.GlobalVersion,
				memRecord.Version,
				memRecord.LogID,
				memRecord.EventName,
				memRecord.Data,
			)

			if !yield(record, nil) {
				return
			}
		}
	}
}
