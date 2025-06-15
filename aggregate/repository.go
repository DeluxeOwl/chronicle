package aggregate

import (
	"context"
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

type ESRepo[TID ID, E event.Any, R Root[TID, E]] struct {
	registry event.Registry
	serde    event.Serializer
	store    event.Log
	newRoot  func() R

	shouldRegisterRoot bool
}

// An implementation of the repo, uses a global type registry and a json serializer.
// It also performs the side effect of registering the root aggregate into the registry (use the option to not do that if you wish).
func NewESRepo[TID ID, E event.Any, R Root[TID, E]](
	eventLog event.Log,
	newRoot func() R,
	opts ...ESRepoOption,
) (*ESRepo[TID, E, R], error) {
	esr := &ESRepo[TID, E, R]{
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

func (repo *ESRepo[TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return repo.GetVersion(ctx, id, version.SelectFromBeginning)
}

func (repo *ESRepo[TID, E, R]) GetVersion(ctx context.Context, id TID, selector version.Selector) (R, error) {
	root := repo.newRoot()

	if err := ReadAndLoadFromStore(ctx, root, repo.store, repo.registry, repo.serde, id, selector); err != nil {
		return emptyRoot[R](), fmt.Errorf("repo get: %w", err)
	}

	return root, nil
}

func (repo *ESRepo[TID, E, R]) Save(ctx context.Context, root R) (version.Version, event.CommitedEvents, error) {
	newVersion, commitedEvents, err := CommitEvents(ctx, repo.store, repo.serde, root)
	if err != nil {
		return newVersion, commitedEvents, fmt.Errorf("repo save: %w", err)
	}
	return newVersion, commitedEvents, nil
}

func (esr *ESRepo[TID, E, R]) setRegistry(r event.Registry) {
	esr.registry = r
}

func (esr *ESRepo[TID, E, R]) setSerializer(s event.Serializer) {
	esr.serde = s
}

func (esr *ESRepo[TID, E, R]) setShouldRegisterRoot(b bool) {
	esr.shouldRegisterRoot = b
}

// Note: we do it this way because otherwise go can't infer the type.
type esRepoConfigurator interface {
	setRegistry(r event.Registry)
	setSerializer(s event.Serializer)
	setShouldRegisterRoot(b bool)
}

type ESRepoOption func(esRepoConfigurator)

func Registry(registry event.Registry) ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setRegistry(registry)
	}
}

func Serializer(serializer event.Serializer) ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setSerializer(serializer)
	}
}

func DontRegisterRoot() ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setShouldRegisterRoot(false)
	}
}
