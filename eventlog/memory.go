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
	events         map[event.LogID][]memStoreRecord
	logVersions    map[event.LogID]version.Version
	globalVersion  version.Version
	mu             sync.RWMutex
	tailingEnabled bool
	cond           *sync.Cond
}

type memStoreRecord struct {
	LogID         event.LogID     `json:"logID"`
	EventName     string          `json:"eventName"`
	Data          []byte          `json:"data"`
	Version       version.Version `json:"version"`
	GlobalVersion version.Version `json:"globalVersion"`
}

type MemoryOption func(*Memory)

// WithMemoryGlobalTailing enables the "tailing" or "subscription" mode for ReadAllEvents.
// When enabled, the iterator will block and wait for new events after reading
// all historical ones, behaving like a channel.
func WithMemoryGlobalTailing() MemoryOption {
	return func(m *Memory) {
		m.tailingEnabled = true
	}
}

// NewMemory creates a new in-memory event store.
func NewMemory(opts ...MemoryOption) *Memory {
	mem := &Memory{
		mu:             sync.RWMutex{},
		events:         make(map[event.LogID][]memStoreRecord),
		logVersions:    make(map[event.LogID]version.Version),
		globalVersion:  version.Zero,
		cond:           nil,
		tailingEnabled: false,
	}

	for _, opt := range opts {
		opt(mem)
	}

	// If tailing is enabled, initialize the condition variable.
	// It uses the RWMutex as its Locker, which is required for Wait/Broadcast.
	if mem.tailingEnabled {
		mem.cond = sync.NewCond(&mem.mu)
	}

	return mem
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

	err := fn(ctx, MemTx{})

	// If tailing is enabled, signal any waiting ReadAllEvents iterators that new
	// data may be available. This must be done while the lock is still held.
	if mem.tailingEnabled {
		mem.cond.Broadcast()
	}

	return err
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

		for _, internalEncoded := range events {
			record := store.memoryRecordToRecord(&internalEncoded)

			if record.Version() < selector.From {
				continue
			}

			if selector.To > 0 && record.Version() > selector.To {
				break
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

func (mem *Memory) DangerouslyDeleteEventsUpTo(
	ctx context.Context,
	id event.LogID,
	version version.Version,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	mem.mu.Lock()
	defer mem.mu.Unlock()

	events, ok := mem.events[id]
	if !ok {
		return nil
	}

	n := 0
	for _, rec := range events {
		if rec.Version > version {
			events[n] = rec
			n++
		}
	}
	mem.events[id] = events[:n]

	return nil
}

// ReadAllEvents returns an iterator over all events in the store.
// If the store was created with WithGlobalTailing(), the iterator will first yield
// all existing events and then block indefinitely, yielding new events as they are appended.
func (mem *Memory) ReadAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	// If tailing is not enabled, use the original, non-blocking implementation.
	if !mem.tailingEnabled {
		return mem.readAllEventsOnce(ctx, globalSelector)
	}
	// Otherwise, use the blocking/subscription implementation.
	return mem.subscribeToAllEvents(ctx, globalSelector)
}

func (mem *Memory) readAllEventsOnce(
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
			if globalSelector.To > 0 && memRecord.GlobalVersion > globalSelector.To {
				break
			}
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
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

// subscribeToAllEvents implements the blocking "tail" behavior.
//
//nolint:funlen,gocognit // will refactor.
func (mem *Memory) subscribeToAllEvents(
	ctx context.Context,
	globalSelector version.Selector,
) event.GlobalRecords {
	return func(yield func(*event.GlobalRecord, error) bool) {
		var lastVersionSeen version.Version
		if globalSelector.From > 0 {
			lastVersionSeen = globalSelector.From - 1
		}

		mem.mu.RLock()
		initialEvents := make([]memStoreRecord, 0)
		for _, logEvents := range mem.events {
			initialEvents = append(initialEvents, logEvents...)
		}
		mem.mu.RUnlock()

		sort.Slice(initialEvents, func(i, j int) bool {
			return initialEvents[i].GlobalVersion < initialEvents[j].GlobalVersion
		})

		for _, memRecord := range initialEvents {
			if memRecord.GlobalVersion <= lastVersionSeen {
				continue
			}
			if globalSelector.To > 0 && memRecord.GlobalVersion > globalSelector.To {
				break
			}
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
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
			lastVersionSeen = memRecord.GlobalVersion
		}

		if globalSelector.To > 0 && lastVersionSeen >= globalSelector.To {
			return
		}

		// This goroutine's job is to wake up the main loop when the context is cancelled.
		if ctx.Done() != nil {
			go func() {
				<-ctx.Done()         // Block until context is cancelled.
				mem.cond.Broadcast() // Wake up any waiting iterators.
			}()
		}

		for {
			mem.mu.Lock()

			// Find new events that occurred since our last check.
			var newEvents []memStoreRecord
			for _, logEvents := range mem.events {
				for i := range logEvents {
					if logEvents[i].GlobalVersion > lastVersionSeen {
						newEvents = append(newEvents, logEvents[i])
					}
				}
			}

			// Loop while there are no new events AND the context is not yet cancelled.
			for len(newEvents) == 0 && ctx.Err() == nil {
				// Atomically unlocks mem.mu, waits for a signal, and re-locks mem.mu.
				mem.cond.Wait()

				// After waking up (either from new data or cancellation), re-scan for events.
				for _, logEvents := range mem.events {
					for i := range logEvents {
						if logEvents[i].GlobalVersion > lastVersionSeen {
							newEvents = append(newEvents, logEvents[i])
						}
					}
				}
			}

			// We are out of the wait loop. This can happen for two reasons:
			// 1. New events have arrived.
			// 2. The context was cancelled.

			// Check for cancellation first, as it's the exit condition.
			if err := ctx.Err(); err != nil {
				mem.mu.Unlock()
				yield(nil, err)
				return
			}

			// If we got here, it must be because of new events.
			mem.mu.Unlock()

			sort.Slice(newEvents, func(i, j int) bool {
				return newEvents[i].GlobalVersion < newEvents[j].GlobalVersion
			})

			for _, memRecord := range newEvents {
				if globalSelector.To > 0 && memRecord.GlobalVersion > globalSelector.To {
					return
				}
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
				lastVersionSeen = memRecord.GlobalVersion
			}

			if globalSelector.To > 0 && lastVersionSeen >= globalSelector.To {
				return
			}
		}
	}
}
