package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var _ Repository[testAggID, testAggEvent, *testAgg] = (*ESRepoWithSnapshots[testAggID, testAggEvent, *testAgg, *testAgg])(
	nil,
)

// ESRepoWithSnapshots is a decorator for a Repository that adds a snapshotting
// capability to improve performance for aggregates with long event histories.
//
// When retrieving an aggregate, it first attempts to load from a recent snapshot
// and then replays only the events that occurred since. When saving, it commits
// events and then, based on a configured strategy, creates and stores a new
// snapshot of the aggregate's state.
type ESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] struct {
	internal Repository[TID, E, R]

	snapstore        SnapshotStore[TID, TS]
	snapshotter      Snapshotter[TID, E, R, TS]
	onSnapshotErr    OnSnapshotErrFunc
	snapshotStrategy SnapshotStrategy[TID, E, R]

	snapshotSaveEnabled bool
}

// OnSnapshotErrFunc defines the signature for a function that handles errors
// occurring during the saving of a snapshot.
//
// Since snapshots are a performance optimization and not the source of truth,
// it is often acceptable to log the error and continue without returning it,
// preventing a snapshot failure from failing the entire operation.
//
// Usage:
//
//	// Configure the repository to log snapshot errors but not fail the Save operation.
//	option := aggregate.OnSnapshotError(func(ctx context.Context, err error) error {
//	    log.Printf("Warning: failed to save snapshot: %v", err)
//	    return nil // Returning nil ignores the error.
//	})
type OnSnapshotErrFunc = func(ctx context.Context, err error) error

// NewESRepoWithSnapshots creates a new repository decorator that adds snapshotting functionality.
//
// Usage:
//
//	// baseRepo is a standard event-sourced repository.
//	// snapStore is an implementation of aggregate.SnapshotStore.
//	// snapshotter is an implementation of aggregate.Snapshotter.
//	repoWithSnaps, err := aggregate.NewESRepoWithSnapshots(
//		baseRepo,
//		snapStore,
//		snapshotter,
//		aggregate.SnapStrategyFor[...].EveryNEvents(50), // Snapshot every 50 events.
//	)
//
// Returns a new repository equipped with snapshotting capabilities, or an error if
// the configuration is invalid.
func NewESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	esRepo Repository[TID, E, R],
	snapstore SnapshotStore[TID, TS],
	snapshotter Snapshotter[TID, E, R, TS],
	snapstrategy SnapshotStrategy[TID, E, R],
	opts ...ESRepoWithSnapshotsOption,
) (*ESRepoWithSnapshots[TID, E, R, TS], error) {
	esr := &ESRepoWithSnapshots[TID, E, R, TS]{
		internal: esRepo,
		onSnapshotErr: func(ctx context.Context, err error) error {
			return err
		},
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

// Get retrieves an aggregate's state. It first attempts to load the aggregate from
// the most recent snapshot. If a snapshot is found, it then replays only the events
// that occurred after that snapshot's version. If no snapshot is found, it falls
// back to replaying the aggregate's entire event history.
//
// Usage:
//
//	// The repository will automatically use a snapshot if available.
//	account, err := repo.Get(ctx, accountID)
//
// Returns the fully constituted aggregate root and an error if loading fails.
func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Get(ctx context.Context, id TID) (R, error) {
	var empty R

	root, found, err := LoadFromSnapshot(ctx, esr.snapstore, esr.snapshotter, id)
	if err != nil {
		return empty, fmt.Errorf("snapshot repo get: could not retrieve snapshot: %w", err)
	}

	if !found {
		return esr.internal.Get(ctx, id)
	}

	if err := esr.internal.LoadAggregate(ctx, root, id, version.Selector{
		From: root.Version() + 1,
	}); err != nil {
		return empty, fmt.Errorf("snapshot repo get: failed to load events after snapshot: %w", err)
	}

	return root, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) GetVersion(
	ctx context.Context,
	id TID,
	selector version.Selector,
) (R, error) {
	return esr.internal.GetVersion(ctx, id, selector)
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) LoadAggregate(
	ctx context.Context,
	root R,
	id TID,
	selector version.Selector,
) error {
	return esr.internal.LoadAggregate(ctx, root, id, selector)
}

// Save persists the uncommitted events of an aggregate. After successfully saving
// the events, it runs the configured SnapshotStrategy to determine whether
// a new snapshot of the aggregate's state should also be saved.
//
// Usage:
//
//	account.DepositMoney(100)
//	// Events are saved, and a snapshot may be created and saved.
//	_, _, err := repo.Save(ctx, account)
//
// Returns the new version of the aggregate, the list of committed events, and an
// error if either event persistence or (if configured to) snapshot persistence fails.
func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommittedEvents[E], error) {
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
	if err != nil && esr.onSnapshotErr != nil {
		var snapshotErr error
		if snapshotErr = esr.onSnapshotErr(ctx, err); snapshotErr != nil {
			snapshotErr = fmt.Errorf("snapshot repo save: save snapshot: %w", snapshotErr)
		}
		return newVersion, committedEvents, snapshotErr
	}

	return newVersion, committedEvents, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setOnSnapshotErr(fn OnSnapshotErrFunc) {
	esr.onSnapshotErr = fn
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setSnapshotSaveEnabled(enabled bool) {
	esr.snapshotSaveEnabled = enabled
}

type esRepoWithSnapshotsConfigurator interface {
	setOnSnapshotErr(OnSnapshotErrFunc)
	setSnapshotSaveEnabled(bool)
}

type ESRepoWithSnapshotsOption func(esRepoWithSnapshotsConfigurator)

// OnSnapshotError provides an option to set a custom handler for snapshot save errors.
//
// Usage:
//
//	// Configure the repository to log and ignore snapshot save errors
//	repo, err := aggregate.NewESRepoWithSnapshots(
//	    ...,
//	    aggregate.OnSnapshotError(func(ctx context.Context, err error) error {
//	        log.Printf("Failed to save snapshot: %v", err)
//	        return nil // nil means the error is handled and won't be returned by Save
//	    }),
//	)
func OnSnapshotError(fn OnSnapshotErrFunc) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setOnSnapshotErr(fn)
	}
}

// SnapshotSaveEnabled provides an option to enable or disable the saving of snapshots.
// This can be useful for diagnostics or for scenarios where snapshotting is controlled
// by a separate process. The default is true.
//
// Usage:
//
//	// Disable snapshot saving for this repository instance.
//	repo, err := aggregate.NewESRepoWithSnapshots(
//	    ...,
//	    aggregate.SnapshotSaveEnabled(false),
//	)
func SnapshotSaveEnabled(enabled bool) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setSnapshotSaveEnabled(enabled)
	}
}
