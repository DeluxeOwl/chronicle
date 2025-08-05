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

	log        event.Log
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
		log:                event.NewLogWithProcessor(log, nil),
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
		log:                event.NewTransactableLogWithProcessor(transactor, txLog, nil),
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
func (repo *TransactionalRepository[T, TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommitedEvents[E], error) {
	var newVersion version.Version
	var committedEvents CommitedEvents[E]

	uncommittedEvents := FlushUncommitedEvents(root)

	if len(uncommittedEvents) == 0 {
		return root.Version(), nil, nil // Nothing to save
	}

	err := repo.transactor.WithinTx(ctx, func(ctx context.Context, tx T) error {
		logID := event.LogID(root.ID().String())
		expectedVersion := version.CheckExact(
			root.Version() - version.Version(len(uncommittedEvents)),
		)

		rawEvents, err := uncommittedEvents.ToRaw(repo.serde)
		if err != nil {
			return fmt.Errorf("transactional repo save: events to raw: %w", err)
		}

		// Append events to the log *within the transaction*
		v, _, err := repo.txLog.AppendInTx(ctx, tx, logID, expectedVersion, rawEvents)
		if err != nil {
			return fmt.Errorf("transactional repo save: append in tx: %w", err)
		}

		newVersion = v
		committedEvents = CommitedEvents[E](uncommittedEvents)

		// Call the high-level, type-safe aggregate processor
		if repo.aggProcessor != nil {
			if err := repo.aggProcessor.Process(ctx, tx, root, committedEvents); err != nil {
				return fmt.Errorf("transactional repo save: aggregate processor: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return version.Zero, nil, err
	}

	return newVersion, committedEvents, nil
}

func (repo *TransactionalRepository[T, TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return repo.GetVersion(ctx, id, version.SelectFromBeginning)
}

func (repo *TransactionalRepository[T, TID, E, R]) GetVersion(
	ctx context.Context,
	id TID,
	selector version.Selector,
) (R, error) {
	root := repo.createRoot()
	if err := ReadAndLoadFromStore(ctx, root, repo.log, repo.registry, repo.serde, id, selector); err != nil {
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
