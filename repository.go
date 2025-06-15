package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

func NewEventSourcedRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...aggregate.ESRepoOption,
) (*aggregate.ESRepo[TID, E, R], error) {
	return aggregate.NewESRepo(eventLog, newRoot, opts...)
}

func NewEventSourcedRepositoryWithSnapshots[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E], TS aggregate.Snapshot[TID]](
	eventLog event.Log,
	newRoot func() R,
	snapshotStore aggregate.SnapshotStore[TID, TS],
	snapshotter aggregate.Snapshotter[TID, E, R, TS],
	opts ...aggregate.ESRepoWithSnapshotsOption,
) (*aggregate.ESRepoWithSnapshots[TID, E, R, TS], error) {
	return aggregate.NewESRepoWithSnapshots(eventLog, newRoot, snapshotStore, snapshotter, opts...)
}
