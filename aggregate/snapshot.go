package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
)

type Snapshot[TID ID] interface {
	IDer[TID]
	Versioner
}

type Snapshotter[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] interface {
	ToSnapshot(R) TS
	FromSnapshot(TS) R
}

type SnapshotStore[TID ID, TS Snapshot[TID]] interface {
	SaveSnapshot(ctx context.Context, snapshot TS) error

	// Returns the snapshot, if it was found and an error if any
	GetSnapshot(ctx context.Context, aggregateID TID) (TS, bool, error)
}

func LoadFromSnapshot[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	ctx context.Context,
	store SnapshotStore[TID, TS],
	snapshotter Snapshotter[TID, E, R, TS],
	aggregateID TID,
) (R, bool, error) {
	snap, found, err := store.GetSnapshot(ctx, aggregateID)
	if err != nil {
		return emptyRoot[R](), found, fmt.Errorf("load from snapshot: get snapshot: %w", err)
	}

	if !found {
		return emptyRoot[R](), false, nil
	}

	root := snapshotter.FromSnapshot(snap)
	root.setVersion(snap.Version())

	return root, true, nil
}
