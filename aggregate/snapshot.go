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

// TODO: Should this return an error as well?
// Careful in person, you don't want to use the constructor.
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
	var empty R

	snap, found, err := store.GetSnapshot(ctx, aggregateID)
	if err != nil {
		return empty, found, fmt.Errorf("load from snapshot: get snapshot: %w", err)
	}

	if !found {
		return empty, false, nil
	}

	root := snapshotter.FromSnapshot(snap)
	root.setVersion(snap.Version())

	return root, true, nil
}
