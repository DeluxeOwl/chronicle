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
	registry     event.Registry
	store        event.Log
	newAggregate func() TRoot
}

type EventSourcedRepositoryOption[TID ID, TE event.EventAny, TR Root[TID, TE]] func(*EventSourcedRepository[TID, TE, TR])

func WithRegistryMemory[TID ID, TE event.EventAny, TR Root[TID, TE]](registry event.Registry) EventSourcedRepositoryOption[TID, TE, TR] {
	return func(esr *EventSourcedRepository[TID, TE, TR]) {
		esr.registry = registry
	}
}

func NewEventSourcedRepository[TypeID ID, TEvent event.EventAny, TRoot Root[TypeID, TEvent]](
	store event.Log,
	newAggregateFunc func() TRoot,
	opts ...EventSourcedRepositoryOption[TypeID, TEvent, TRoot],
) *EventSourcedRepository[TypeID, TEvent, TRoot] {
	esr := &EventSourcedRepository[TypeID, TEvent, TRoot]{
		store:        store,
		newAggregate: newAggregateFunc,
		registry:     event.GlobalRegistry,
	}

	for _, o := range opts {
		o(esr)
	}

	return esr
}

func LoadFromRecordedEvents[TypeID ID, TEvent event.EventAny](root Root[TypeID, TEvent], registry event.Registry, events event.Records) error {
	for e, err := range events {
		if err != nil {
			return fmt.Errorf("load from events: %w", err)
		}

		fact, ok := registry.NewEvent(e.EventName())
		if !ok {
			return errors.New("factory not registered for" + e.EventName())
		}

		ev := fact()
		if err := event.Unmarshal(e.Data(), ev); err != nil {
			return fmt.Errorf("internal unmarshal record data: %w", err)
		}

		anyEvt, ok := ev.(TEvent)
		if !ok {
			return errors.New("internal: this isn't supposed to happen (todo)")
		}

		if err := root.Apply(anyEvt); err != nil {
			return fmt.Errorf("load from events: root apply: %w", err)
		}

		root.setVersion(e.Version())
	}

	return nil
}

// todo: internal .get and GetVersioned
func (repo *EventSourcedRepository[TypeID, TEvent, TRoot]) Get(ctx context.Context, id TypeID) (TRoot, error) {
	var zeroValue TRoot

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, version.SelectFromBeginning)

	root := repo.newAggregate()

	if err := LoadFromRecordedEvents(root, repo.registry, recordedEvents); err != nil {
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

	rawEvents, err := event.ConvertEventsToRaw(events)
	if err != nil {
		return fmt.Errorf("aggregate save: events to raw: %w", err)
	}

	if _, err := repo.store.AppendEvents(ctx, logID, expectedVersion, rawEvents); err != nil {
		return fmt.Errorf("aggregate save: append events: %w", err)
	}

	return nil
}
