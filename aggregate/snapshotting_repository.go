package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

// TODO: should I add the types here for snapshot and root?
type SnapshottingRepository[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] struct {
	// Embed the existing repo to reuse its Save logic and basic Get logic
	internal *EventSourcedRepo[TID, E, R]

	onSnapshotError func(error) error

	snapshotStore SnapshotStore[TID, TS]
	snapshotter   Snapshotter[TID, E, R, TS]

	// Optional: a policy for when to save snapshots
	snapshotFrequency uint64
}

const SnapshotFrequency = 100

func NewSnapshottingRepository[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	esr *EventSourcedRepo[TID, E, R],
	snapshotStore SnapshotStore[TID, TS],
	snapshotter Snapshotter[TID, E, R, TS],
) *SnapshottingRepository[TID, E, R, TS] {
	return &SnapshottingRepository[TID, E, R, TS]{
		internal:          esr,
		onSnapshotError:   func(err error) error { return nil },
		snapshotStore:     snapshotStore,
		snapshotter:       snapshotter,
		snapshotFrequency: SnapshotFrequency,
	}
}

func (r *SnapshottingRepository[TID, E, R, TS]) Get(ctx context.Context, id TID) (R, error) {
	var zeroValue R

	root, found, err := LoadFromSnapshot(ctx, r.snapshotStore, r.snapshotter, id)
	if err != nil {
		return zeroValue, fmt.Errorf("snapshot repo get: could not retrieve snapshot: %w", err)
	}

	if !found {
		return r.internal.Get(ctx, id)
	}

	if err := ReadAndLoadFromStore(ctx, root, r.internal.store, r.internal.registry, r.internal.serde, id, version.Selector{
		From: root.Version() + 1,
	}); err != nil {
		return zeroValue, fmt.Errorf("snapshot repo get: failed to load events after snapshot: %w", err)
	}

	return root, nil
}

// Save persists the uncommitted events of an aggregate and, if the policy dictates,
// creates and saves a new snapshot of the aggregate's state.
func (r *SnapshottingRepository[TID, E, R, TS]) Save(ctx context.Context, root R) (version.Version, event.CommitedEvents, error) {
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
