package aggregate

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"
	"github.com/DeluxeOwl/eventuallynow/version"
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

type RepositoryComposition[TypeID ID, TRoot Root[TypeID]] interface {
	Getter[TypeID, TRoot]
	Saver[TypeID, TRoot]
}

// TODO: return domain errors, like the event payload that didn't work
func LoadFromEvents[TypeID ID](root Root[TypeID], events event.StoredEvents) error {
	for event := range events {
		if err := root.Apply(event.Payload); err != nil {
			return fmt.Errorf("aggregate.LoadFromEvents: failed to load events, %w", err)
		}
		root.setVersion(event.Version)
	}

	return nil
}

type EventSourcedRepository[TypeID ID, TRoot Root[TypeID]] struct {
	// Every event sourced repo is backed by a generic log
	store event.Log
	kind  Type[TypeID, TRoot]
}

func NewEventSourcedRepository[TypeID ID, TRoot Root[TypeID]](store event.Log, kind Type[TypeID, TRoot]) EventSourcedRepository[TypeID, TRoot] {
	return EventSourcedRepository[TypeID, TRoot]{
		store: store,
		kind:  kind,
	}
}

func (repo EventSourcedRepository[TypeID, TRoot]) Get(ctx context.Context, id TypeID) (TRoot, error) {
	var zeroValue TRoot

	logID := event.LogID(id.String())
	storedEvents := repo.store.ReadEvents(ctx, logID, version.SelectFromBeginning)

	root := repo.kind.Factory()

	if err := LoadFromEvents(root, storedEvents); err != nil {
		return zeroValue, fmt.Errorf("load events: %w", err)
	}

	if root.Version() == 0 {
		return zeroValue, errors.New("root not found")
	}

	return root, nil
}

func (repo EventSourcedRepository[TypeID, TRoot]) Save(ctx context.Context, root TRoot) error {
	events := root.FlushRecordedEvents()
	if len(events) == 0 {
		return nil
	}

	logID := event.LogID(root.ID().String())

	// This gets the version that the root started with before accumulating events.
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(events)),
	)

	if _, err := repo.store.AppendEvents(ctx, logID, expectedVersion, events...); err != nil {
		return fmt.Errorf("commit recorded events: %w", err)
	}

	return nil
}
