package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"

	"github.com/DeluxeOwl/chronicle/version"
)

//go:generate go run github.com/matryer/moq@latest -pkg aggregate_test -skip-ensure -rm -out repository_mock_test.go . Repository TransactionalAggregateProcessor

// Getter is responsible for retrieving the latest state of an aggregate root.
// It loads all events for the given ID and replays them to reconstruct the aggregate.
//
// Usage:
//
//	// repo implements Getter
//	account, err := repo.Get(ctx, "account-123")
//
// Returns the fully hydrated aggregate root or an error if the aggregate is not found or loading fails.
type Getter[TID ID, E event.Any, R Root[TID, E]] interface {
	Get(ctx context.Context, id TID) (R, error)
}

// VersionedGetter is responsible for retrieving an aggregate root at a specific version.
// It loads and replays events up to the version specified in the selector.
//
// Usage:
//
//	// repo implements VersionedGetter
//	// Get the state of the account after its 5th event
//	account, err := repo.GetVersion(ctx, "account-123", version.Selector{From: 1, To: 5})
//
// Returns the hydrated aggregate root at the specified version or an error.
type VersionedGetter[TID ID, E event.Any, R Root[TID, E]] interface {
	GetVersion(ctx context.Context, id TID, selector version.Selector) (R, error)
}

// Saver is responsible for persisting the uncommitted events of an aggregate root.
//
// Usage:
//
//	// repo implements Saver
//	newVersion, committedEvents, err := repo.Save(ctx, myAccount)
//
// Returns the new version of the aggregate, the events that were committed, and an
// error if saving fails (e.g., a `version.ConflictError`).
type Saver[TID ID, E event.Any, R Root[TID, E]] interface {
	Save(ctx context.Context, root R) (version.Version, CommittedEvents[E], error)
}

// AggregateLoader is an interface for loading events and applying them to an *existing*
// instance of an aggregate root. This is primarily used by the snapshotting mechanism,
// which first creates an aggregate from a snapshot, then uses this interface to load
// and apply only the new events that occurred after the snapshot was taken.
//
// Usage (internal, used by repository with snapshots):
//
//	// repo implements AggregateLoader
//	// root is an aggregate created from a snapshot at version 10
//	err := repo.LoadAggregate(ctx, root, "id-123", version.Selector{From: 11})
//
// Returns an error if loading or applying subsequent events fails.
type AggregateLoader[TID ID, E event.Any, R Root[TID, E]] interface {
	LoadAggregate(
		ctx context.Context,
		root R,
		id TID,
		selector version.Selector,
	) error
}

// Repository combines the core operations for loading and saving an aggregate root.
// It is the primary interface for interacting with aggregates.
type Repository[TID ID, E event.Any, R Root[TID, E]] interface {
	AggregateLoader[TID, E, R]
	VersionedGetter[TID, E, R]
	Getter[TID, E, R]
	Saver[TID, E, R]
}

// FusedRepo is a convenience type that implements the Repository interface by
// composing its constituent parts. It is useful for creating repository decorators,
// such as a retry wrapper, where you might only want to override one behavior (like Save)
// while keeping the others.
//
// Usage:
//
//	// Wrap a standard repository's Save method with a custom retry mechanism
//	baseRepo := ...
//	repoWithRetry := &FusedRepo[...]{
//	    AggregateLoader: baseRepo,
//	    VersionedGetter: baseRepo,
//	    Getter:          baseRepo,
//	    Saver:           &MyCustomSaverWithRetry{saver: baseRepo},
//	}
type FusedRepo[TID ID, E event.Any, R Root[TID, E]] struct {
	AggregateLoader[TID, E, R]
	VersionedGetter[TID, E, R]
	Getter[TID, E, R]
	Saver[TID, E, R]
}

var _ Repository[testAggID, testAggEvent, *testAgg] = (*ESRepo[testAggID, testAggEvent, *testAgg])(
	nil,
)

// ESRepo is the standard event-sourced repository implementation.
// It orchestrates loading and saving aggregates by interacting with an event log
// and an event registry. By default, it uses JSON for serialization.
type ESRepo[TID ID, E event.Any, R Root[TID, E]] struct {
	registry   event.Registry[E]
	serde      serde.BinarySerde
	eventlog   event.Log
	createRoot func() R

	transformers []event.Transformer[E]

	shouldRegisterRoot bool
}

// NewESRepo creates a new event sourced repository.
// It requires an event log for storage, a factory function to create new aggregate
// instances, and an optional slice of event transformers. By default, it uses a JSON
// serializer and automatically registers the aggregate's events.
//
// Usage:
//
//	repo, err := NewESRepo(
//	    eventlog.NewMemory(),
//	    account.NewEmpty,     // func() *account.Account
//	    nil,                  // No transformers
//	)
//
// Returns a fully initialized ESRepo or an error if event registration fails.
func NewESRepo[TID ID, E event.Any, R Root[TID, E]](
	eventLog event.Log,
	createRoot func() R,
	transformers []event.Transformer[E],
	opts ...ESRepoOption,
) (*ESRepo[TID, E, R], error) {
	esr := &ESRepo[TID, E, R]{
		eventlog:           eventLog,
		createRoot:         createRoot,
		registry:           event.NewRegistry[E](),
		serde:              serde.NewJSONBinary(),
		transformers:       transformers,
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

// LoadAggregate loads events from the store and applies them to an existing aggregate instance.
// See `AggregateLoader` for more details.
func (repo *ESRepo[TID, E, R]) LoadAggregate(
	ctx context.Context,
	root R,
	id TID,
	selector version.Selector,
) error {
	return ReadAndLoadFromStore(
		ctx,
		root,
		repo.eventlog,
		repo.registry,
		repo.serde,
		repo.transformers,
		id,
		selector,
	)
}

// Get retrieves the latest state of an aggregate by creating a new instance
// and replaying all of its events.
func (repo *ESRepo[TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return repo.GetVersion(ctx, id, version.SelectFromBeginning)
}

// GetVersion retrieves the state of an aggregate at a specific version.
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

// Save commits all uncommitted events from the aggregate to the event log.
func (repo *ESRepo[TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommittedEvents[E], error) {
	newVersion, committedEvents, err := CommitEvents(
		ctx,
		repo.eventlog,
		repo.serde,
		repo.transformers,
		root,
	)
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

// ESRepoOption is a function that configures an ESRepo instance.
// It is used with `NewESRepo` to customize its behavior.
type ESRepoOption func(esRepoConfigurator)

// EventSerializer provides an option to override the default JSON serializer
// with a custom implementation. See the `serde` package.
//
// Usage:
//
//	repo, err := NewESRepo(..., aggregate.EventSerializer(myCustomSerializer))
func EventSerializer(serializer serde.BinarySerde) ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setSerializer(serializer)
	}
}

// DontRegisterRoot provides an option to prevent the repository from automatically
// registering the aggregate's events. This is useful when you manage event registration
// centrally or use a shared registry.
//
// Usage:
//
//	repo, err := NewESRepo(..., aggregate.DontRegisterRoot())
func DontRegisterRoot() ESRepoOption {
	return func(c esRepoConfigurator) {
		c.setShouldRegisterRoot(false)
	}
}

// AnyEventRegistry provides an option to use a shared, global event registry.
// This allows multiple repositories to share a single registry, preventing duplicate
// event name registrations across different aggregates.
//
// Usage:
//
//	globalRegistry := event.NewRegistry[event.Any]()
//	repo, err := NewESRepo(..., aggregate.AnyEventRegistry(globalRegistry))
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
