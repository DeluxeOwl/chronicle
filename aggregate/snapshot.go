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

func LoadFromSnapshot[TypeID ID, E event.Any, TSnapshot Snapshot[TypeID]](
	ctx context.Context,
	store SnapshotStore[TypeID, TSnapshot],
	snapshotter Snapshotter[TypeID, E, Root[TypeID, E], Snapshot[TypeID]],
	aggregateID TypeID,
) (Root[TypeID, E], bool, error) {
	snap, found, err := store.GetSnapshot(ctx, aggregateID)
	if err != nil {
		return nil, found, fmt.Errorf("load from snapshot: get snapshot: %w", err)
	}

	if !found {
		return nil, false, nil
	}

	root := snapshotter.FromSnapshot(snap)
	root.setVersion(snap.Version())

	return root, true, nil
}
