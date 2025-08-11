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

type VersionedGetter[TID ID, E event.Any, R Root[TID, E]] interface {
	GetVersion(ctx context.Context, id TID, selector version.Selector) (R, error)
}

type Saver[TID ID, E event.Any, R Root[TID, E]] interface {
	Save(ctx context.Context, root R) (version.Version, CommittedEvents[E], error)
}

// This is needed for the snapshot to be able to hydrate an aggregate in case
// we found an older snapshot and we need to replay the events starting with the next version
type AggregateLoader[TID ID, E event.Any, R Root[TID, E]] interface {
	LoadAggregate(
		ctx context.Context,
		root R,
		id TID,
		selector version.Selector,
	) error
}

type Repository[TID ID, E event.Any, R Root[TID, E]] interface {
	AggregateLoader[TID, E, R]
	VersionedGetter[TID, E, R]
	Getter[TID, E, R]
	Saver[TID, E, R]
}

// FusedRepo is a convenience type that can be used to fuse together
// different implementations for the Getter and Saver Repository interface components.
// Useful for wrapping the Save method with a retry for optimistic concurrency control.
type FusedRepo[TID ID, E event.Any, R Root[TID, E]] struct {
	AggregateLoader[TID, E, R]
	VersionedGetter[TID, E, R]
	Getter[TID, E, R]
	Saver[TID, E, R]
}

var _ Repository[testAggID, testAggEvent, *testAgg] = (*ESRepo[testAggID, testAggEvent, *testAgg])(
	nil,
)

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

func (repo *ESRepo[TID, E, R]) LoadAggregate(
	ctx context.Context,
	root R,
	id TID,
	selector version.Selector,
) error {
	return ReadAndLoadFromStore(ctx, root, repo.eventlog, repo.registry, repo.serde, id, selector)
}

func (repo *ESRepo[TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return repo.GetVersion(ctx, id, version.SelectFromBeginning)
}

func (repo *ESRepo[TID, E, R]) GetVersion(
	ctx context.Context,
	id TID,
	selector version.Selector,
) (R, error) {
	root := repo.createRoot()

	if err := repo.LoadAggregate(ctx, root, id, selector); err != nil {
		var empty R
		return empty, fmt.Errorf("repo get version %d: %w", selector.From, err)
	}

	return root, nil
}

func (repo *ESRepo[TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommittedEvents[E], error) {
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

// Aggregate for compile time check

type testAggID string

func (testAggID) String() string { return "" }

var _ Root[testAggID, testAggEvent] = (*testAgg)(nil)

type testAgg struct {
	Base
}
type testAggEvent interface {
	event.Any
}

// Apply implements Root.
func (t *testAgg) Apply(e testAggEvent) error {
	panic("unimplemented")
}

// EventFuncs implements Root.
func (t *testAgg) EventFuncs() event.FuncsFor[testAggEvent] {
	panic("unimplemented")
}

// ID implements Root.
func (t *testAgg) ID() testAggID {
	panic("unimplemented")
}
