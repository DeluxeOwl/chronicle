package aggregate

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Repository[TID ID, E event.Any, R Root[TID, E]] struct {
	registry event.Registry
	store    event.Log
	newRoot  func() R
}

type RepositoryOption[TID ID, E event.Any, R Root[TID, E]] func(*Repository[TID, E, R])

func WithRegistry[TID ID, E event.Any, R Root[TID, E]](
	registry event.Registry,
) RepositoryOption[TID, E, R] {
	return func(esr *Repository[TID, E, R]) {
		esr.registry = registry
	}
}

func NewRepository[TID ID, E event.Any, R Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...RepositoryOption[TID, E, R],
) (*Repository[TID, E, R], error) {
	esr := &Repository[TID, E, R]{
		store:    eventLog,
		newRoot:  newRoot,
		registry: event.GlobalRegistry,
	}

	for _, o := range opts {
		o(esr)
	}

	err := esr.registry.RegisterRoot(newRoot())
	if err != nil {
		return nil, fmt.Errorf("new aggregate repository: %w", err)
	}

	return esr, nil
}

func (repo *Repository[TID, E, R]) Get(ctx context.Context, id ID) (R, error) {
	return repo.get(ctx, id, version.SelectFromBeginning)
}

func (repo *Repository[TID, E, R]) GetVersioned(ctx context.Context, id ID, selector version.Selector) (R, error) {
	return repo.get(ctx, id, selector)
}

func (repo *Repository[TID, E, R]) get(ctx context.Context, id ID, selector version.Selector) (R, error) {
	var zeroValue R

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, selector)

	root := repo.newRoot()

	if err := LoadFromRecords(root, repo.registry, recordedEvents); err != nil {
		return zeroValue, err
	}

	if root.Version() == 0 {
		return zeroValue, errors.New("root not found")
	}

	return root, nil
}

func (repo *Repository[TID, E, R]) Save(ctx context.Context, root R) error {
	// Theoretically, if we wanted to allow custom repo
	// we could make it like so: any(root).(UncommitedEventsFlusher)
	uncommitedEvents := root.flushUncommitedEvents()

	if len(uncommitedEvents) == 0 {
		return nil
	}

	logID := event.LogID(root.ID().String())
	expectedVersion := version.CheckExact(
		root.Version() - version.Version(len(uncommitedEvents)),
	)

	rawEvents, err := uncommitedEvents.ToRaw()
	if err != nil {
		return fmt.Errorf("aggregate save: events to raw: %w", err)
	}

	if _, err := repo.store.AppendEvents(ctx, logID, expectedVersion, rawEvents); err != nil {
		return fmt.Errorf("aggregate save: append events: %w", err)
	}

	return nil
}
