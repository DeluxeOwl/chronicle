package aggregate

import (
	"context"
	"fmt"

	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/serde"
	"github.com/DeluxeOwl/chronicle/version"
)

type TransactionalRepository[T any, TID ID, E event.Any, R Root[TID, E]] struct {
	transactor event.Transactor[T]
	txLog      event.TransactionalLog[T]

	eventlog   event.Log
	serde      serde.BinarySerde
	registry   event.Registry[E]
	createRoot func() R

	aggProcessor TransactionalAggregateProcessor[T, TID, E, R]

	shouldRegisterRoot bool
}

// NewTransactionalRepository creates a repository that manages operations within an atomic transaction.
// It requires a Transactor and a TransactionalLog to control the transaction boundary.
func NewTransactionalRepository[TX any, TID ID, E event.Any, R Root[TID, E]](
	log event.TransactionalEventLog[TX],
	createRoot func() R,
	aggProcessor TransactionalAggregateProcessor[TX, TID, E, R],
	opts ...ESRepoOption,
) (*TransactionalRepository[TX, TID, E, R], error) {
	repo := &TransactionalRepository[TX, TID, E, R]{
		transactor:         log,
		txLog:              log,
		eventlog:           event.NewLogWithProcessor(log, nil),
		createRoot:         createRoot,
		registry:           event.NewRegistry[E](),
		serde:              serde.NewJSONBinary(),
		shouldRegisterRoot: true,
		aggProcessor:       aggProcessor,
	}

	for _, o := range opts {
		o(repo)
	}

	if repo.shouldRegisterRoot {
		if err := repo.registry.RegisterEvents(createRoot()); err != nil {
			return nil, fmt.Errorf("new transactional repository: %w", err)
		}
	}

	return repo, nil
}

func NewTransactionalRepositoryWithTransactor[TX any, TID ID, E event.Any, R Root[TID, E]](
	transactor event.Transactor[TX],
	txLog event.TransactionalLog[TX],
	createRoot func() R,
	aggProcessor TransactionalAggregateProcessor[TX, TID, E, R],
	opts ...ESRepoOption,
) (*TransactionalRepository[TX, TID, E, R], error) {
	repo := &TransactionalRepository[TX, TID, E, R]{
		transactor:         transactor,
		txLog:              txLog,
		eventlog:           event.NewTransactableLogWithProcessor(transactor, txLog, nil),
		createRoot:         createRoot,
		registry:           event.NewRegistry[E](),
		serde:              serde.NewJSONBinary(),
		shouldRegisterRoot: true,
		aggProcessor:       aggProcessor,
	}

	for _, o := range opts {
		o(repo)
	}
	if repo.shouldRegisterRoot {
		if err := repo.registry.RegisterEvents(createRoot()); err != nil {
			return nil, fmt.Errorf("new transactional repository: %w", err)
		}
	}

	return repo, nil
}

// Save atomically persists the aggregate's events and executes any configured
// transactional processors within a single database transaction.
func (repo *TransactionalRepository[TX, TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommitedEvents[E], error) {
	newVersion, committedEvents, err := CommitEventsWithTX(
		ctx,
		repo.transactor,
		repo.txLog,
		repo.aggProcessor,
		repo.serde,
		root,
	)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf("repo save: %w", err)
	}

	return newVersion, committedEvents, nil
}

func (repo *TransactionalRepository[TX, TID, E, R]) HydrateAggregate(
	ctx context.Context,
	root R,
	id TID,
	selector version.Selector,
) error {
	return ReadAndLoadFromStore(ctx, root, repo.eventlog, repo.registry, repo.serde, id, selector)
}

func (repo *TransactionalRepository[TX, TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	root := repo.createRoot()

	if err := repo.HydrateAggregate(ctx, root, id, version.SelectFromBeginning); err != nil {
		var empty R
		return empty, fmt.Errorf("repo get: %w", err)
	}

	return root, nil
}

func (repo *TransactionalRepository[T, TID, E, R]) setSerializer(s serde.BinarySerde) {
	repo.serde = s
}

func (repo *TransactionalRepository[T, TID, E, R]) setShouldRegisterRoot(b bool) {
	repo.shouldRegisterRoot = b
}

func (repo *TransactionalRepository[T, TID, E, R]) setAnyRegistry(
	anyRegistry event.Registry[event.Any],
) {
	repo.registry = event.NewConcreteRegistryFromAny[E](anyRegistry)
}
