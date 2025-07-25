package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
)

//go:generate go run github.com/matryer/moq@latest -pkg aggregate_test -skip-ensure -rm -out snapshot_mock_test.go . SnapshotStore

type Snapshot[TID ID] interface {
	IDer[TID]
	Versioner
}

type Snapshotter[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] interface {
	ToSnapshot(R) (TS, error)
	FromSnapshot(TS) (R, error)
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

	root, err := snapshotter.FromSnapshot(snap)
	if err != nil {
		return empty, found, fmt.Errorf("load from snapshot: convert from snapshot: %w", err)
	}
	root.setVersion(snap.Version())

	return root, true, nil
}
