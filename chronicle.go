package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/aggregate/snapshotstore"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/event/eventlog"
)

// Registries.
func NewEventRegistry[E event.Any]() *event.EventRegistry[E] {
	return event.NewRegistry[E]()
}

func NewAnyEventRegistry() *event.EventRegistry[event.Any] {
	return event.NewRegistry[event.Any]()
}

// Event logs.
func NewEventLogMemory() *eventlog.Memory {
	return eventlog.NewMemory()
}

// Snapshot stores.
func NewSnapshotStoreMemory[TID aggregate.ID, TS aggregate.Snapshot[TID]](
	createSnapshot func() TS,
	opts ...snapshotstore.MemoryStoreOption[TID, TS],
) *snapshotstore.MemoryStore[TID, TS] {
	return snapshotstore.NewMemoryStore(createSnapshot, opts...)
}

// Repositories.
func NewEventSourcedRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventlog event.Log,
	createRoot func() R,
	opts ...aggregate.ESRepoOption,
) (*aggregate.ESRepo[TID, E, R], error) {
	return aggregate.NewESRepo(eventlog, createRoot, opts...)
}

func NewEventSourcedRepositoryWithSnapshots[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E], TS aggregate.Snapshot[TID]](
	eventlog event.Log,
	snapstore aggregate.SnapshotStore[TID, TS],
	createRoot func() R,
	snapshotter aggregate.Snapshotter[TID, E, R, TS],
	snapstrategy aggregate.SnapshotStrategy[TID, E, R],
	opts ...aggregate.ESRepoWithSnapshotsOption,
) (*aggregate.ESRepoWithSnapshots[TID, E, R, TS], error) {
	return aggregate.NewESRepoWithSnapshots(eventlog, snapstore, createRoot, snapshotter, snapstrategy, opts...)
}
