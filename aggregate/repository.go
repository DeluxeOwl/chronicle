package aggregate

import (
	"context"
	"errors"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

type Getter[TID ID, E event.Any, R Root[TID, E]] interface {
	Get(ctx context.Context, id TID) (R, error)
	GetVersion(ctx context.Context, id ID, selector version.Selector) (R, error)
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

	shouldRegisterRoot bool
}

// Note: we do it this way because otherwise go can't infer the type.
type repoConfigurator interface {
	setRegistry(r event.Registry)
	setSerializer(s event.Serializer)
	setShouldRegisterRoot(b bool)
}

type EventSourcedRepoOption func(repoConfigurator)

func Registry(registry event.Registry) EventSourcedRepoOption {
	return func(c repoConfigurator) {
		c.setRegistry(registry)
	}
}

func Serializer(serializer event.Serializer) EventSourcedRepoOption {
	return func(c repoConfigurator) {
		c.setSerializer(serializer)
	}
}

func DontRegisterRoot() EventSourcedRepoOption {
	return func(c repoConfigurator) {
		c.setShouldRegisterRoot(false)
	}
}

// An implementation of the repo, uses a global type registry and a json serializer.
// It also performs the side effect of registering the root aggregate into the registry (use the option to not do that if you wish).
func NewEventSourcedRepo[TID ID, E event.Any, R Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...EventSourcedRepoOption,
) (*EventSourcedRepo[TID, E, R], error) {
	esr := &EventSourcedRepo[TID, E, R]{
		store:              eventLog,
		newRoot:            newRoot,
		registry:           event.GlobalRegistry,
		serde:              event.NewJSONSerializer(),
		shouldRegisterRoot: true,
	}

	for _, o := range opts {
		o(esr)
	}

	if esr.shouldRegisterRoot {
		err := esr.registry.RegisterRoot(newRoot())
		if err != nil {
			return nil, fmt.Errorf("new aggregate repository: %w", err)
		}
	}

	return esr, nil
}

func (repo *EventSourcedRepo[TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return repo.GetVersion(ctx, id, version.SelectFromBeginning)
}

func (repo *EventSourcedRepo[TID, E, R]) GetVersion(ctx context.Context, id TID, selector version.Selector) (R, error) {
	var zeroValue R

	logID := event.LogID(id.String())
	recordedEvents := repo.store.ReadEvents(ctx, logID, selector)

	root := repo.newRoot()

	if err := LoadFromRecords(root, repo.registry, repo.serde, recordedEvents); err != nil {
		return zeroValue, fmt.Errorf("repo get: %w", err)
	}

	if root.Version() == 0 {
		return zeroValue, errors.New("root not found")
	}

	return root, nil
}

func (repo *EventSourcedRepo[TID, E, R]) Save(ctx context.Context, root R) (version.Version, event.CommitedEvents, error) {
	newVersion, commitedEvents, err := CommitEvents(ctx, repo.store, repo.serde, root)
	if err != nil {
		return newVersion, commitedEvents, fmt.Errorf("repo save: %w", err)
	}
	return newVersion, commitedEvents, nil
}

func (esr *EventSourcedRepo[TID, E, R]) setRegistry(r event.Registry) {
	esr.registry = r
}

func (esr *EventSourcedRepo[TID, E, R]) setSerializer(s event.Serializer) {
	esr.serde = s
}

func (esr *EventSourcedRepo[TID, E, R]) setShouldRegisterRoot(b bool) {
	esr.shouldRegisterRoot = b
}
