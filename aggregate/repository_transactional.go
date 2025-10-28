package aggregate

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/DeluxeOwl/chronicle/encoding"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/version"
)

var _ Repository[testAggID, testAggEvent, *testAgg] = (*TransactionalRepository[*sql.Tx, testAggID, testAggEvent, *testAgg])(
	nil,
)

// TransactionalRepository is a repository implementation that ensures that saving an aggregate's events
// and processing them (e.g., for updating projections) occur within a single, atomic transaction.
// It orchestrates operations using an `event.Transactor` and an `event.TransactionalLog`, and can
// execute a type-safe `TransactionalAggregateProcessor` as part of the same transaction.
// This is the useful for implementing strongly consistent read models or the transactional outbox pattern.
type TransactionalRepository[T any, TID ID, E event.Any, R Root[TID, E]] struct {
	transactor event.Transactor[T]
	txLog      event.TransactionalLog[T]

	eventlog     event.Log
	encoder      encoding.Codec
	registry     event.Registry[E]
	createRoot   func() R
	transformers []event.Transformer[E]

	aggProcessor TransactionalAggregateProcessor[T, TID, E, R]

	shouldRegisterRoot bool
}

// NewTransactionalRepository creates a repository that manages operations within an atomic transaction.
// This constructor is a convenience for when the event log implementation (like the provided Postgres or Sqlite logs)
// also serves as the transaction manager by implementing `event.TransactionalEventLog`.
//
// Usage:
//
//	// postgresLog implements event.TransactionalEventLog[*sql.Tx]
//	// myProcessor implements aggregate.TransactionalAggregateProcessor for *sql.Tx
//	repo, err := aggregate.NewTransactionalRepository(
//	    postgresLog,
//	    account.NewEmpty,
//	    nil, // no transformers
//	    myProcessor,
//	)
//
// Returns a new `*TransactionalRepository` configured for atomic operations, or an error if setup fails.
func NewTransactionalRepository[TX any, TID ID, E event.Any, R Root[TID, E]](
	log event.TransactionalEventLog[TX],
	createRoot func() R,
	transformers []event.Transformer[E],
	aggProcessor TransactionalAggregateProcessor[TX, TID, E, R],
	opts ...ESRepoOption,
) (*TransactionalRepository[TX, TID, E, R], error) {
	repo := &TransactionalRepository[TX, TID, E, R]{
		transactor:         log,
		txLog:              log,
		eventlog:           event.NewLogWithProcessor(log, nil),
		createRoot:         createRoot,
		registry:           event.NewRegistry[E](),
		encoder:            encoding.NewJSONB(),
		shouldRegisterRoot: true,
		aggProcessor:       aggProcessor,
		transformers:       transformers,
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

// NewTransactionalRepositoryWithTransactor creates a transactional repository with a separate transactor and log.
// This constructor provides more flexibility by decoupling the transaction management from the event storage logic.
// It is useful in advanced scenarios where you might use a generic transaction coordinator.
//
// Usage:
//
//	// myTransactor implements event.Transactor[*sql.Tx]
//	// myTxLog implements event.TransactionalLog[*sql.Tx]
//	repo, err := aggregate.NewTransactionalRepositoryWithTransactor(
//	    myTransactor,
//	    myTxLog,
//	    account.NewEmpty,
//	    nil, // no transformers
//	    myProcessor,
//	)
//
// Returns a new `*TransactionalRepository`, or an error if setup fails.
func NewTransactionalRepositoryWithTransactor[TX any, TID ID, E event.Any, R Root[TID, E]](
	transactor event.Transactor[TX],
	txLog event.TransactionalLog[TX],
	createRoot func() R,
	transformers []event.Transformer[E],
	aggProcessor TransactionalAggregateProcessor[TX, TID, E, R],
	opts ...ESRepoOption,
) (*TransactionalRepository[TX, TID, E, R], error) {
	repo := &TransactionalRepository[TX, TID, E, R]{
		transactor:         transactor,
		txLog:              txLog,
		eventlog:           event.NewTransactableLogWithProcessor(transactor, txLog, nil),
		createRoot:         createRoot,
		registry:           event.NewRegistry[E](),
		encoder:            encoding.NewJSONB(),
		shouldRegisterRoot: true,
		aggProcessor:       aggProcessor,
		transformers:       transformers,
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

// Save atomically persists the aggregate's uncommitted events and executes the configured
// transactional processor within a single database transaction. If any part of the process fails,
// the entire transaction is rolled back, ensuring data consistency.
//
// Usage:
//
//	// acc is an aggregate with uncommitted events
//	newVersion, committedEvents, err := transactionalRepo.Save(ctx, acc)
//	if err != nil {
//	    // The transaction was rolled back.
//	    // Handle the conflict or transient error.
//	}
//
// Returns the aggregate's new version and the list of committed events on success.
// Returns an error if saving the events or executing the processor fails.
func (repo *TransactionalRepository[TX, TID, E, R]) Save(
	ctx context.Context,
	root R,
) (version.Version, CommittedEvents[E], error) {
	newVersion, committedEvents, err := CommitEventsWithTX(
		ctx,
		repo.transactor,
		repo.txLog,
		repo.aggProcessor,
		repo.encoder,
		repo.transformers,
		root,
	)
	if err != nil {
		return newVersion, committedEvents, fmt.Errorf("repo save: %w", err)
	}

	return newVersion, committedEvents, nil
}

func (repo *TransactionalRepository[TX, TID, E, R]) LoadAggregate(
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
		repo.encoder,
		repo.transformers,
		id,
		selector,
	)
}

func (repo *TransactionalRepository[TX, TID, E, R]) Get(ctx context.Context, id TID) (R, error) {
	return repo.GetVersion(ctx, id, version.SelectFromBeginning)
}

func (repo *TransactionalRepository[TX, TID, E, R]) GetVersion(
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

func (repo *TransactionalRepository[T, TID, E, R]) setSerializer(s encoding.Codec) {
	repo.encoder = s
}

func (repo *TransactionalRepository[T, TID, E, R]) setShouldRegisterRoot(b bool) {
	repo.shouldRegisterRoot = b
}

func (repo *TransactionalRepository[T, TID, E, R]) setAnyRegistry(
	anyRegistry event.Registry[event.Any],
) {
	repo.registry = event.NewConcreteRegistryFromAny[E](anyRegistry)
}
