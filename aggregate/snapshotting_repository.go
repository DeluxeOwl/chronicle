package aggregate

import (
	"github.com/DeluxeOwl/chronicle/event"
)

type SnapshottingRepository[TID ID, E event.Any] struct {
	// Embed the existing repo to reuse its Save logic and basic Get logic
	internal *EventSourcedRepo[TID, E, Root[TID, E]]

	snapshotStore SnapshotStore[TID, Snapshot[TID]]
	snapshotter   Snapshotter[TID, E, Root[TID, E], Snapshot[TID]]
	// Optional: a policy for when to save snapshots
	snapshotFrequency int
}
