package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/internal/typeutils"
	"github.com/DeluxeOwl/chronicle/version"
)

type ESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]] struct {
	internal      *ESRepo[TID, E, R]
	snapshotStore SnapshotStore[TID, TS]
	snapshotter   Snapshotter[TID, E, R, TS]

	onSnapshotError  OnSnapshotErrorFunc
	snapshotStrategy SnapshotStrategy[TID, E, R]
}

const SnapshotFrequency = 3

type OnSnapshotErrorFunc = func(error) error

func NewESRepoWithSnapshots[TID ID, E event.Any, R Root[TID, E], TS Snapshot[TID]](
	eventLog event.Log,
	newRoot func() R,
	snapshotStore SnapshotStore[TID, TS],
	snapshotter Snapshotter[TID, E, R, TS],
	strategy SnapshotStrategy[TID, E, R],
	opts ...ESRepoWithSnapshotsOption,
) (*ESRepoWithSnapshots[TID, E, R, TS], error) {
	esr := &ESRepoWithSnapshots[TID, E, R, TS]{
		internal: &ESRepo[TID, E, R]{
			store:              eventLog,
			newRoot:            newRoot,
			registry:           event.GlobalRegistry,
			serde:              event.NewJSONSerializer(),
			shouldRegisterRoot: true,
		},
		onSnapshotError:  func(err error) error { return nil },
		snapshotStore:    snapshotStore,
		snapshotter:      snapshotter,
		snapshotStrategy: strategy,
	}

	for _, o := range opts {
		o(esr)
	}

	if esr.internal.shouldRegisterRoot {
		err := esr.internal.registry.RegisterRoot(newRoot())
		if err != nil {
			return nil, fmt.Errorf("new aggregate repository: %w", err)
		}
	}

	return esr, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Get(ctx context.Context, id TID) (R, error) {
	root, found, err := LoadFromSnapshot(ctx, esr.snapshotStore, esr.snapshotter, id)
	if err != nil {
		return typeutils.Zero[R](), fmt.Errorf("snapshot repo get: could not retrieve snapshot: %w", err)
	}

	if !found {
		return esr.internal.Get(ctx, id)
	}

	if err := ReadAndLoadFromStore(ctx,
		root,
		esr.internal.store,
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
func (esr *ESRepoWithSnapshots[TID, E, R, TS]) Save(ctx context.Context, root R) (version.Version, event.CommitedEvents, error) {
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
		if err := esr.snapshotStore.SaveSnapshot(ctx, snapshot); err != nil {
			return newVersion, committedEvents, esr.onSnapshotError(err)
		}
	}

	return newVersion, committedEvents, nil
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setRegistry(r event.Registry) {
	esr.internal.registry = r
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setSerializer(s event.Serializer) {
	esr.internal.serde = s
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setShouldRegisterRoot(b bool) {
	esr.internal.shouldRegisterRoot = b
}

func (esr *ESRepoWithSnapshots[TID, E, R, TS]) setOnSnapshotError(fn OnSnapshotErrorFunc) {
	esr.onSnapshotError = fn
}

type esRepoWithSnapshotsConfigurator interface {
	setRegistry(r event.Registry)
	setSerializer(s event.Serializer)
	setShouldRegisterRoot(b bool)
	setOnSnapshotError(OnSnapshotErrorFunc)
}

type ESRepoWithSnapshotsOption func(esRepoWithSnapshotsConfigurator)

func RegistryS(registry event.Registry) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setRegistry(registry)
	}
}

func SerializerS(serializer event.Serializer) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setSerializer(serializer)
	}
}

func DontRegisterRootS() ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setShouldRegisterRoot(false)
	}
}

func OnSnapshotErrorS(fn OnSnapshotErrorFunc) ESRepoWithSnapshotsOption {
	return func(c esRepoWithSnapshotsConfigurator) {
		c.setOnSnapshotError(fn)
	}
}
