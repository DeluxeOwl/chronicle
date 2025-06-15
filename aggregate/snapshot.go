package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
)

type Snapshot[TypeID ID] interface {
	IDer[TypeID]
	Versioner
}

type Snapshotter[TypeID ID, E event.Any, TRoot Root[TypeID, E], TSnapshot Snapshot[TypeID]] interface {
	ToSnapshot(TRoot) TSnapshot
	FromSnapshot(TSnapshot) TRoot
}

type SnapshotStore[TypeID ID, TSnapshot Snapshot[TypeID]] interface {
	SaveSnapshot(ctx context.Context, snapshot TSnapshot) error

	// Returns the snapshot, if it was found and an error if any
	GetSnapshot(ctx context.Context, aggregateID TypeID) (TSnapshot, bool, error)
}

// TODO: do we need the found? in other places we check root.Version() == 0
func LoadFromSnapshot[TypeID ID, E event.Any, R Root[TypeID, E], TS Snapshot[TypeID]](
	ctx context.Context,
	store SnapshotStore[TypeID, TS],
	snapshotter Snapshotter[TypeID, E, R, TS],
	aggregateID TypeID,
) (R, bool, error) {
	var zeroValue R

	snap, found, err := store.GetSnapshot(ctx, aggregateID)
	if err != nil {
		return zeroValue, found, fmt.Errorf("load from snapshot: get snapshot: %w", err)
	}

	if !found {
		return zeroValue, false, nil
	}

	root := snapshotter.FromSnapshot(snap)
	root.setVersion(snap.Version())

	return root, true, nil
}
