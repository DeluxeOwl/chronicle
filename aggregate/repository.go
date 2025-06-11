package aggregate

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/eventuallynow/event"

	"github.com/DeluxeOwl/eventuallynow/version"
)

// TODO: interface

type EventSourcedRepository[TypeID ID, TEvent event.EventAny, TRoot Root[TypeID, TEvent]] struct {
	store        event.Log
	newAggregate func() TRoot
}

func NewEventSourcedRepository[TypeID ID, TEvent event.EventAny, TRoot Root[TypeID, TEvent]](
	store event.Log,
	newAggregateFunc func() TRoot,
) *EventSourcedRepository[TypeID, TEvent, TRoot] {
	return &EventSourcedRepository[TypeID, TEvent, TRoot]{
		store:        store,
		newAggregate: newAggregateFunc,
	}
}

func LoadFromEvents[TypeID ID, TEvent event.EventAny](root Root[TypeID, TEvent], events event.RecordedEvents) error {
	for event, err := range events {
		if err != nil {
			return fmt.Errorf("load from events: %w", err)
		}

		anyEvt, ok := event.EventAny().(TEvent)
		if !ok {
			return errors.New("internal: this isn't supposed to happen (todo)")
		}

		if err := root.Apply(anyEvt); err != nil {
			return fmt.Errorf("load from events: root apply: %w", err)
		}
		root.setVersion(event.Version())
	}

	return nil
}

func (repo *EventSourcedRepository[TypeID, TEvent, TRoot]) Get(ctx context.Context, id TypeID) (TRoot, error) {
	var zeroValue TRoot

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, version.SelectFromBeginning)

	root := repo.newAggregate()

	if err := LoadFromEvents(root, recordedEvents); err != nil {
		return zeroValue, err
	}

	if root.Version() == 0 {
		return zeroValue, errors.New("root not found")
	}

	return root, nil
}

func (repo *EventSourcedRepository[TypeID, TEvent, TRoot]) Save(ctx context.Context, root TRoot) error {
	events := root.FlushUncommitedEvents()
	if len(events) == 0 {
		return nil
	}

	logID := event.LogID(root.ID().String())
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(events)),
	)

	if _, err := repo.store.AppendEvents(ctx, logID, expectedVersion, events...); err != nil {
		return fmt.Errorf("aggregate save: append events: %w", err)
	}

	return nil
}
