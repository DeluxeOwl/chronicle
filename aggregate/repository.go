package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"

	"github.com/DeluxeOwl/chronicle/version"
)

//go:generate go run github.com/matryer/moq@latest -pkg aggregate_test -skip-ensure -rm -out repository_mock_test.go . Repository TransactionalAggregateProcessor

type Getter[TID ID, E event.Any, R Root[TID, E]] interface {
	Get(ctx context.Context, id TID) (R, error)
}

type Saver[TID ID, E event.Any, R Root[TID, E]] interface {
	Save(ctx context.Context, root R) (version.Version, CommitedEvents[E], error)
}

type Repository[TID ID, E event.Any, R Root[TID, E]] interface {
	Getter[TID, E, R]
	Saver[TID, E, R]
}

type ESRepo[TID ID, E event.Any, R Root[TID, E]] struct {
	registry   event.Registry[E]
	serde      serde.BinarySerde
	eventlog   event.Log
	createRoot func() R

	shouldRegisterRoot bool
}

// An implementation of the repo, uses a global type registry and a json serializer.
// It also performs the side effect of registering the root aggregate into the registry (use the option to not do that if you wish).
func NewESRepo[TID ID, E event.Any, R Root[TID, E]](
	eventLog event.Log,
	createRoot func() R,
	opts ...ESRepoOption,
) (*ESRepo[TID, E, R], error) {
	esr := &ESRepo[TID, E, R]{
		eventlog:           eventLog,
		createRoot:         createRoot,
		registry:           event.NewRegistry[E](),
		serde:              serde.NewJSONBinary(),
		shouldRegisterRoot: true,
	}

	for _, o := range opts {
		o(esr)
	}

	if esr.shouldRegisterRoot {
		err := esr.registry.RegisterEvents(createRoot())
		if err != nil {
			return nil, fmt.Errorf("new aggregate repository: %w", err)
		}
	}

	return esr, nil
}

func (repo *ESRepo[TID, E, R]) HydrateAggregate(
	ctx context.Context,
	root R,
	id TID,
	selector version.Selector,
) error {
	return ReadAndLoadFromStore(ctx, root, repo.eventlog, repo.registry, repo.serde, id, selector)
}

func (repo *ESRepo[TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	root := repo.createRoot()

	if err := repo.HydrateAggregate(ctx, root, id, version.SelectFromBeginning); err != nil {
		var empty R
		return empty, fmt.Errorf("repo get: %w", err)
	}

	return root, nil
}

func (repo *ESRepo[TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommitedEvents[E], error) {
	newVersion, committedEvents, err := CommitEvents(ctx, repo.eventlog, repo.serde, root)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf("repo save: %w", err)
	}
	return newVersion, committedEvents, nil
}

func (esr *ESRepo[TID, E, R]) setSerializer(s serde.BinarySerde) {
	esr.serde = s
}

func (esr *ESRepo[TID, E, R]) setShouldRegisterRoot(b bool) {
	esr.shouldRegisterRoot = b
}

func (esr *ESRepo[TID, E, R]) setAnyRegistry(anyRegistry event.Registry[event.Any]) {
	esr.registry = event.NewConcreteRegistryFromAny[E](anyRegistry)
}

// Note: we do it this way because otherwise go can't infer the type.
type esRepoConfigurator interface {
	setSerializer(s serde.BinarySerde)
	setShouldRegisterRoot(b bool)
	setAnyRegistry(anyRegistry event.Registry[event.Any])
}

type ESRepoOption func(esRepoConfigurator)

func EventSerializer(serializer serde.BinarySerde) ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setSerializer(serializer)
	}
}

func DontRegisterRoot() ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setShouldRegisterRoot(false)
	}
}

func AnyEventRegistry(anyRegistry event.Registry[event.Any]) ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setAnyRegistry(anyRegistry)
	}
}
