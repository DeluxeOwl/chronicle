package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type SnapshottingRepository[TID ID, E event.Any] struct {
	// Embed the existing repo to reuse its Save logic and basic Get logic
	internal *EventSourcedRepo[TID, E, Root[TID, E]]

	onSnapshotError func(error) error

	snapshotStore SnapshotStore[TID, Snapshot[TID]]
	snapshotter   Snapshotter[TID, E, Root[TID, E], Snapshot[TID]]

	// Optional: a policy for when to save snapshots
	snapshotFrequency uint64
}

const SnapshotFrequency = 100

func NewSnapshottingRepository[TID ID, E event.Any](
	esr *EventSourcedRepo[TID, E, Root[TID, E]],
	snapshotStore SnapshotStore[TID, Snapshot[TID]],
	snapshotter Snapshotter[TID, E, Root[TID, E], Snapshot[TID]],
) *SnapshottingRepository[TID, E] {
	return &SnapshottingRepository[TID, E]{
		internal:          esr,
		onSnapshotError:   func(err error) error { return nil },
		snapshotStore:     snapshotStore,
		snapshotter:       snapshotter,
		snapshotFrequency: SnapshotFrequency,
	}
}

func (r *SnapshottingRepository[TID, E]) Get(ctx context.Context, id TID) (Root[TID, E], error) {
	var zeroValue Root[TID, E]

	root, found, err := LoadFromSnapshot(ctx, r.snapshotStore, r.snapshotter, id)
	if err != nil {
		return zeroValue, fmt.Errorf("snapshot repo get: could not retrieve snapshot: %w", err)
	}

	if !found {
		return r.internal.Get(ctx, id)
	}

	logID := event.LogID(id.String())
	records := r.internal.store.ReadEvents(ctx, logID, version.Selector{
		From: root.Version() + 1,
	})

	if err := LoadFromRecords(root, r.internal.registry, r.internal.serde, records); err != nil {
		return zeroValue, fmt.Errorf("snapshot repo get: failed to load events after snapshot: %w", err)
	}

	return root, nil
}

// Save persists the uncommitted events of an aggregate and, if the policy dictates,
// creates and saves a new snapshot of the aggregate's state.
func (r *SnapshottingRepository[TID, E]) Save(ctx context.Context, root Root[TID, E]) (version.Version, event.CommitedEvents, error) {
	// First, commit events to the event log. This is the source of truth.
	newVersion, committedEvents, err := r.internal.Save(ctx, root)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf("snapshot repo save: %w", err)
	}

	if uint64(newVersion)%r.snapshotFrequency == 0 {
		snapshot := r.snapshotter.ToSnapshot(root)
		if err := r.snapshotStore.SaveSnapshot(ctx, snapshot); err != nil {
			return newVersion, committedEvents, r.onSnapshotError(err)
		}
	}

	return newVersion, committedEvents, nil
}
