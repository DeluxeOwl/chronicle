package aggregate

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Getter[TID ID, E event.Any, R Root[TID, E]] interface {
	Get(ctx context.Context, id ID) (R, error)
}

type Saver[TID ID, E event.Any, R Root[TID, E]] interface {
	Save(ctx context.Context, root R) error
}

type Repository[TID ID, E event.Any, R Root[TID, E]] interface {
	Getter[TID, E, R]
	Saver[TID, E, R]
}

type EventSourcedRepo[TID ID, E event.Any, R Root[TID, E]] struct {
	registry event.Registry
	serde    event.Serializer
	store    event.Log
	newRoot  func() R
}

type EventSourcedRepoOption[TID ID, E event.Any, R Root[TID, E]] func(*EventSourcedRepo[TID, E, R])

func WithRegistry[TID ID, E event.Any, R Root[TID, E]](
	registry event.Registry,
) EventSourcedRepoOption[TID, E, R] {
	return func(esr *EventSourcedRepo[TID, E, R]) {
		esr.registry = registry
	}
}

func WithSerializer[TID ID, E event.Any, R Root[TID, E]](
	serializer event.Serializer,
) EventSourcedRepoOption[TID, E, R] {
	return func(esr *EventSourcedRepo[TID, E, R]) {
		esr.serde = serializer
	}
}

// An implementation of the repo, uses a global type registry and a json serializer.
func NewEventSourcedRepo[TID ID, E event.Any, R Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...EventSourcedRepoOption[TID, E, R],
) (*EventSourcedRepo[TID, E, R], error) {
	esr := &EventSourcedRepo[TID, E, R]{
		store:    eventLog,
		newRoot:  newRoot,
		registry: event.GlobalRegistry,
		serde:    event.NewJSONSerializer(),
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

func (repo *EventSourcedRepo[TID, E, R]) Get(ctx context.Context, id ID) (R, error) {
	return repo.getFromVersion(ctx, id, version.SelectFromBeginning)
}

// Would a public method for this help?
func (repo *EventSourcedRepo[TID, E, R]) getFromVersion(ctx context.Context, id ID, selector version.Selector) (R, error) {
	var zeroValue R

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, selector)

	root := repo.newRoot()

	if err := LoadFromRecords(root, repo.registry, repo.serde, recordedEvents); err != nil {
		return zeroValue, err
	}

	if root.Version() == 0 {
		return zeroValue, errors.New("root not found")
	}

	return root, nil
}

func (repo *EventSourcedRepo[TID, E, R]) Save(ctx context.Context, root R) error {
	return CommitEvents(ctx, root, repo.serde, repo.store)
}
