package chronicle

// TODO: rename package to "es"

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type AggregateRepository[ID aggregate.ID, E event.EventAny, R aggregate.Root[ID, E]] struct {
	registry event.Registry
	store    event.Log
	newRoot  func() R
}

type AggregateRepositoryOption[ID aggregate.ID, E event.EventAny, R aggregate.Root[ID, E]] func(*AggregateRepository[ID, E, R])

func WithRegistry[ID aggregate.ID, E event.EventAny, R aggregate.Root[ID, E]](
	registry event.Registry,
) AggregateRepositoryOption[ID, E, R] {
	return func(esr *AggregateRepository[ID, E, R]) {
		esr.registry = registry
	}
}

func NewAggregateRepository[ID aggregate.ID, E event.EventAny, R aggregate.Root[ID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...AggregateRepositoryOption[ID, E, R],
) *AggregateRepository[ID, E, R] {
	esr := &AggregateRepository[ID, E, R]{
		store:    eventLog,
		newRoot:  newRoot,
		registry: event.GlobalRegistry,
	}

	for _, o := range opts {
		o(esr)
	}

	esr.registry.RegisterRoot(newRoot())

	return esr
}

// todo: internal .get and GetVersioned
func (repo *AggregateRepository[ID, E, R]) Get(ctx context.Context, id ID) (R, error) {
	var zeroValue R

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, version.SelectFromBeginning)

	root := repo.newRoot()

	if err := aggregate.LoadFromRecordedEvents(root, repo.registry, recordedEvents); err != nil {
		return zeroValue, err
	}

	if root.Version() == 0 {
		return zeroValue, errors.New("root not found")
	}

	return root, nil
}

func (repo *AggregateRepository[ID, E, R]) Save(ctx context.Context, root R) error {
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
