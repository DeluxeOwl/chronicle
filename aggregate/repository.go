package aggregate

import (
	"context"

	"github.com/DeluxeOwl/eventuallynow/event"

	"github.com/DeluxeOwl/eventuallynow/version"
	"github.com/DeluxeOwl/zerrors"
)

const (
	ErrLoadFromEvents     AggregateError = "failed_load_from_events"
	ErrRehydrateFromState AggregateError = "failed_rehydrate_from_state"
	ErrRootNotFound       AggregateError = "root_not_found"
	ErrCommitEvents       AggregateError = "commit_recorded_events"
)

type Getter[TypeID ID, TRoot Root[TypeID]] interface {
	Get(ctx context.Context, id TypeID) (TRoot, error)
}

type Saver[TypeID ID, TRoot Root[TypeID]] interface {
	Save(ctx context.Context, root TRoot) error
}

type Repository[TypeID ID, TRoot Root[TypeID]] interface {
	Getter[TypeID, TRoot]
	Saver[TypeID, TRoot]
}

type EventSourcedRepository[TypeID ID, TRoot Root[TypeID]] struct {
	store        event.Log
	newAggregate func() TRoot
}

func NewEventSourcedRepository[TypeID ID, TRoot Root[TypeID]](
	store event.Log,
	newAggregateFunc func() TRoot,
) *EventSourcedRepository[TypeID, TRoot] {
	return &EventSourcedRepository[TypeID, TRoot]{
		store:        store,
		newAggregate: newAggregateFunc,
	}
}

func LoadFromEvents[TypeID ID](root Root[TypeID], events event.RecordedEvents) error {
	for event, err := range events {
		if err != nil {
			return zerrors.New(ErrLoadFromEvents).WithError(err)
		}
		if err := root.Apply(event.Unwrap()); err != nil {
			return zerrors.New(ErrLoadFromEvents).WithError(err)
		}
		root.setVersion(event.Version())
	}

	return nil
}

func (repo *EventSourcedRepository[TypeID, TRoot]) Get(ctx context.Context, id TypeID) (TRoot, error) {
	var zeroValue TRoot

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, version.SelectFromBeginning)

	root := repo.newAggregate()

	if err := LoadFromEvents(root, recordedEvents); err != nil {
		return zeroValue, err
	}

	if root.Version() == 0 {
		return zeroValue, zerrors.New(ErrRootNotFound)
	}

	return root, nil
}

func (repo *EventSourcedRepository[TypeID, TRoot]) Save(ctx context.Context, root TRoot) error {
	events := root.FlushRecordedEvents()
	if len(events) == 0 {
		return nil
	}

	logID := event.LogID(root.ID().String())
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(events)),
	)

	if _, err := repo.store.AppendEvents(ctx, logID, expectedVersion, events...); err != nil {
		return zerrors.New(ErrCommitEvents).WithError(err)
	}

	return nil
}
