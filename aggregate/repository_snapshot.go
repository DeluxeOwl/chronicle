package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/internal/typeutils"
	"github.com/DeluxeOwl/chronicle/version"
)

type ESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] struct {
	internal    *ESRepo[TID, E, R]
	snapstore   SnapshotStore[TID, TS]
	snapshotter Snapshotter[TID, E, R, TS]

	returnSnapshotErr ReturnSnapshotErrFunc
	snapshotStrategy  SnapshotStrategy[TID, E, R]
}

type ReturnSnapshotErrFunc = func(error) error

func NewESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	eventlog event.Log,
	snapstore SnapshotStore[TID, TS],
	createRoot func() R,
	snapshotter Snapshotter[TID, E, R, TS],
	snapstrategy SnapshotStrategy[TID, E, R],
	opts ...ESRepoWithSnapshotsOption,
) (*ESRepoWithSnapshots[TID, E, R, TS], error) {
	esr := &ESRepoWithSnapshots[TID, E, R, TS]{
		internal: &ESRepo[TID, E, R]{
			eventlog:           eventlog,
			createRoot:         createRoot,
			registry:           event.NewRegistry[E](),
			serde:              event.NewJSONSerializer(),
			shouldRegisterRoot: true,
		},
		returnSnapshotErr: func(err error) error { return nil },
		snapstore:         snapstore,
		snapshotter:       snapshotter,
		snapshotStrategy:  snapstrategy,
	}

	for _, o := range opts {
		o(esr)
	}

	if esr.internal.shouldRegisterRoot {
		err := esr.internal.registry.RegisterRoot(createRoot())
		if err != nil {
			return nil, fmt.Errorf("new aggregate repository: %w", err)
		}
	}

	return esr, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Get(ctx context.Context, id TID) (R, error) {
	root, found, err := LoadFromSnapshot(ctx, esr.snapstore, esr.snapshotter, id)
	if err != nil {
		return typeutils.Zero[R](), fmt.Errorf("snapshot repo get: could not retrieve snapshot: %w", err)
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
		return typeutils.Zero[R](), fmt.Errorf("snapshot repo get: failed to load events after snapshot: %w", err)
	}

	return root, nil
}

// Save persists the uncommitted events of an aggregate and, if the policy dictates,
// creates and saves a new snapshot of the aggregate's state.
func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Save(ctx context.Context, root R) (version.Version, event.CommitedEvents[E], error) {
	// First, commit events to the event log. This is the source of truth.
	newVersion, committedEvents, err := esr.internal.Save(ctx, root)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf("snapshot repo save: %w", err)
	}

	if len(committedEvents) == 0 {
		return newVersion, committedEvents, nil // Nothing to do
	}

	previousVersion := newVersion - version.Version(len(committedEvents))

	if esr.snapshotStrategy.ShouldSnapshot(ctx, root, previousVersion, newVersion, committedEvents) {
		snapshot := esr.snapshotter.ToSnapshot(root)
		if err := esr.snapstore.SaveSnapshot(ctx, snapshot); err != nil {
			return newVersion, committedEvents, esr.returnSnapshotErr(err)
		}
	}

	return newVersion, committedEvents, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setSerializer(s event.Serializer) {
	esr.internal.serde = s
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setShouldRegisterRoot(b bool) {
	esr.internal.shouldRegisterRoot = b
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setReturnSnapshotErr(fn ReturnSnapshotErrFunc) {
	esr.returnSnapshotErr = fn
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setAnyRegistry(anyRegistry event.Registry[event.Any]) {
	esr.internal.registry = event.NewConcreteRegistryFromAny[E](anyRegistry)
}

type esRepoWithSnapshotsConfigurator interface {
	setSerializer(s event.Serializer)
	setShouldRegisterRoot(b bool)
	setReturnSnapshotErr(ReturnSnapshotErrFunc)
	setAnyRegistry(anyRegistry event.Registry[event.Any])
}

type ESRepoWithSnapshotsOption func(esRepoWithSnapshotsConfigurator)

func SnapEventSerializer(serializer event.Serializer) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setSerializer(serializer)
	}
}

func SnapDontRegisterRoot() ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setShouldRegisterRoot(false)
	}
}

func SnapReturnError(fn ReturnSnapshotErrFunc) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setReturnSnapshotErr(fn)
	}
}

func SnapAnyEventRegistry(anyRegistry event.Registry[event.Any]) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setAnyRegistry(anyRegistry)
	}
}
