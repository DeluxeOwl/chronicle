package aggregate

import (
	"context"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Snapshotter[TypeID ID, E event.Any, TAgg Aggregate[TypeID, E], TSnapshot any] interface {
	ToSnapshot(TAgg) TSnapshot
	FromSnapshot(TSnapshot) TAgg
}

type SnapshotStore[TypeID ID, TSnapshot any] interface {
	SaveSnapshot(ctx context.Context, aggregateID TypeID, version version.Version, snapshot TSnapshot) error

	// Returns the snapshot, if it was found and an error if any
	GetSnapshot(ctx context.Context, aggregateID TypeID) (TSnapshot, bool, error)
}
