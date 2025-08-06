package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type ESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] struct {
	internal          *ESRepo[TID, E, R]
	snapstore         SnapshotStore[TID, TS]
	snapshotter       Snapshotter[TID, E, R, TS]
	returnSnapshotErr ReturnSnapshotErrFunc
	snapshotStrategy  SnapshotStrategy[TID, E, R]

	snapshotSaveEnabled bool
}

// Note: this is a function, in case the user wants to customize the behavior of returning an error.
type ReturnSnapshotErrFunc = func(err error) error

func NewESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	esRepo *ESRepo[TID, E, R],
	snapstore SnapshotStore[TID, TS],
	snapshotter Snapshotter[TID, E, R, TS],
	snapstrategy SnapshotStrategy[TID, E, R],
	opts ...ESRepoWithSnapshotsOption,
) (*ESRepoWithSnapshots[TID, E, R, TS], error) {
	esr := &ESRepoWithSnapshots[TID, E, R, TS]{
		internal:            esRepo,
		returnSnapshotErr:   func(err error) error { return nil },
		snapstore:           snapstore,
		snapshotter:         snapshotter,
		snapshotStrategy:    snapstrategy,
		snapshotSaveEnabled: true,
	}

	for _, o := range opts {
		o(esr)
	}

	return esr, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Get(ctx context.Context, id TID) (R, error) {
	var empty R

	root, found, err := LoadFromSnapshot(ctx, esr.snapstore, esr.snapshotter, id)
	if err != nil {
		return empty, fmt.Errorf("snapshot repo get: could not retrieve snapshot: %w", err)
	}

	if !found {
		return esr.internal.Get(ctx, id)
	}

	if err := ReadAndLoadFromStore(ctx,
		root,
		esr.internal.eventlog,
		esr.internal.registry,
		esr.internal.serde,
		id,
		version.Selector{
			From: root.Version() + 1,
		}); err != nil {
		return empty, fmt.Errorf("snapshot repo get: failed to load events after snapshot: %w", err)
	}

	return root, nil
}

// Save persists the uncommitted events of an aggregate and, if the policy dictates,
// creates and saves a new snapshot of the aggregate's state.
func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommitedEvents[E], error) {
	// First, commit events to the event log. This is the source of truth.
	newVersion, committedEvents, err := esr.internal.Save(ctx, root)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf("snapshot repo save: %w", err)
	}

	if len(committedEvents) == 0 {
		return newVersion, committedEvents, nil // Nothing to do
	}

	if !esr.snapshotSaveEnabled {
		return newVersion, committedEvents, nil
	}

	previousVersion := newVersion - version.Version(len(committedEvents))

	if !esr.snapshotStrategy.ShouldSnapshot(
		ctx,
		root,
		previousVersion,
		newVersion,
		committedEvents,
	) {
		return newVersion, committedEvents, nil
	}

	snapshot, err := esr.snapshotter.ToSnapshot(root)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf(
			"snapshot repo save: convert to snapshot: %w",
			err,
		)
	}

	err = esr.snapstore.SaveSnapshot(ctx, snapshot)
	if err != nil && esr.returnSnapshotErr != nil {
		var snapshotErr error
		if snapshotErr = esr.returnSnapshotErr(err); snapshotErr != nil {
			snapshotErr = fmt.Errorf("snapshot repo save: save snapshot: %w", snapshotErr)
		}
		return newVersion, committedEvents, snapshotErr
	}

	return newVersion, committedEvents, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setReturnSnapshotErr(fn ReturnSnapshotErrFunc) {
	esr.returnSnapshotErr = fn
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setSnapshotSaveEnabled(enabled bool) {
	esr.snapshotSaveEnabled = enabled
}

type esRepoWithSnapshotsConfigurator interface {
	setReturnSnapshotErr(ReturnSnapshotErrFunc)
	setSnapshotSaveEnabled(bool)
}

type ESRepoWithSnapshotsOption func(esRepoWithSnapshotsConfigurator)

func ReturnErrorFunc(fn ReturnSnapshotErrFunc) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setReturnSnapshotErr(fn)
	}
}

// In case you don't want the snapshot to be saved here (and you're saving the snapshot in another place).
func SnapshotSaveEnabled(enabled bool) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setSnapshotSaveEnabled(enabled)
	}
}
