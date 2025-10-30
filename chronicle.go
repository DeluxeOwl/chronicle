package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/avast/retry-go/v4"
)

// NewEventRegistry creates a new, empty event registry for a specific event base type.
//
// Usage:
//
//	registry := chronicle.NewEventRegistry[account.AccountEvent]()
//
// Returns a pointer to a new EventRegistry.
func NewEventRegistry[E event.Any]() *event.EventRegistry[E] {
	return event.NewRegistry[E]()
}

// NewAnyEventRegistry creates a new, empty event registry for a any event.
//
// Usage:
//
//		registry := chronicle.NewAnyEventRegistry()
//	 	esRepo, err := chronicle.NewEventSourcedRepository(
//			 	memlog,
//			 	NewEmpty,
//			 	nil,
//			 	aggregate.AnyEventRegistry(registry),
//		 )
//
// Returns a pointer to a new EventRegistry.
func NewAnyEventRegistry() *event.EventRegistry[event.Any] {
	return event.NewRegistry[event.Any]()
}

// NewEventSourcedRepository creates a new event sourced repository.
// It requires an event log for storage, a factory function to create new aggregate
// instances, and an optional slice of event transformers. By default, it uses a JSON
// encoder and automatically registers the aggregate's events.
//
// Usage:
//
//	repo, err := chronicle.NewEventSourcedRepository(
//	    eventlog.NewMemory(),
//	    account.NewEmpty,     // func() *account.Account
//	    nil,                  // No transformers
//	)
//
// Returns a fully initialized *aggregate.ESRepo or an error if event registration fails.
func NewEventSourcedRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventlog event.Log,
	createRoot func() R,
	transformers []event.Transformer[E],
	opts ...aggregate.ESRepoOption,
) (*aggregate.ESRepo[TID, E, R], error) {
	return aggregate.NewESRepo(eventlog, createRoot, transformers, opts...)
}

// NewEventSourcedRepositoryWithSnapshots creates a new repository decorator that adds snapshotting functionality.
//
// Usage:
//
//	// baseRepo is a standard event-sourced repository.
//	// snapStore is an implementation of aggregate.SnapshotStore.
//	// snapshotter is an implementation of aggregate.Snapshotter.
//	repoWithSnaps, err := chronicle.NewEventSourcedRepositoryWithSnapshots(
//		baseRepo,
//		snapStore,
//		snapshotter,
//		aggregate.SnapPolicyFor[...].EveryNEvents(50), // Snapshot every 50 events.
//	)
//
// Returns a new repository equipped with snapshotting capabilities, or an error if
// the configuration is invalid.
func NewEventSourcedRepositoryWithSnapshots[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E], TS aggregate.Snapshot[TID]](
	esRepo aggregate.Repository[TID, E, R],
	snapstore aggregate.SnapshotStore[TID, TS],
	snapshotter aggregate.Snapshotter[TID, E, R, TS],
	snapPolicy aggregate.SnapshotPolicy[TID, E, R],
	opts ...aggregate.ESRepoWithSnapshotsOption,
) (*aggregate.ESRepoWithSnapshots[TID, E, R, TS], error) {
	return aggregate.NewESRepoWithSnapshots(
		esRepo,
		snapstore,
		snapshotter,
		snapPolicy,
		opts...)
}

// NewEventSourcedRepositoryWithRetry wraps an existing repository with retry logic.
//
// It takes the repository to be wrapped and an optional, variadic list of
// `retry.Option`s from `github.com/avast/retry-go/v4` to customize the
// retry strategy (e.g., number of attempts, delay, backoff). If no options
// are provided, it defaults to 3 attempts on conflict errors.
//
// Returns a new `*aggregate.ESRepoWithRetry` instance.
func NewEventSourcedRepositoryWithRetry[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	repo aggregate.Repository[TID, E, R],
	opts ...retry.Option,
) *aggregate.ESRepoWithRetry[TID, E, R] {
	return aggregate.NewESRepoWithRetry(repo, opts...)
}

// NewTransactionalRepository creates a repository that manages operations within an atomic transaction.
// This constructor is a convenience for when the event log implementation (like the provided Postgres or Sqlite logs)
// also serves as the transaction manager by implementing `event.TransactionalEventLog`.
//
// Usage:
//
//	// postgresLog implements event.TransactionalEventLog[*sql.Tx]
//	// myProcessor implements aggregate.TransactionalAggregateProcessor for *sql.Tx
//	repo, err := chronicle.NewTransactionalRepository(
//	    postgresLog,
//	    account.NewEmpty,
//	    nil, // no transformers
//	    myProcessor,
//	)
//
// Returns a new `*aggregate.TransactionalRepository` configured for atomic operations, or an error if setup fails.
func NewTransactionalRepository[TX any, TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	log event.TransactionalEventLog[TX],
	createRoot func() R,
	transformers []event.Transformer[E],
	aggProcessor aggregate.TransactionalAggregateProcessor[TX, TID, E, R],
	opts ...aggregate.ESRepoOption,
) (*aggregate.TransactionalRepository[TX, TID, E, R], error) {
	return aggregate.NewTransactionalRepository(
		log,
		createRoot,
		transformers,
		aggProcessor,
		opts...)
}

// NewTransactionalRepositoryWithTransactor creates a transactional repository with a separate transactor and log.
// This constructor provides more flexibility by decoupling the transaction management from the event storage logic.
// It is useful in advanced scenarios where you might use a generic transaction coordinator.
//
// Usage:
//
//	// myTransactor implements event.Transactor[*sql.Tx]
//	// myTxLog implements event.TransactionalLog[*sql.Tx]
//	repo, err := chronicle.NewTransactionalRepositoryWithTransactor(
//	    myTransactor,
//	    myTxLog,
//	    account.NewEmpty,
//	    nil, // no transformers
//	    myProcessor,
//	)
//
// Returns a new `*aggregate.TransactionalRepository`, or an error if setup fails.
func NewTransactionalRepositoryWithTransactor[TX any, TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	transactor event.Transactor[TX],
	txLog event.TransactionalLog[TX],
	createRoot func() R,
	transformers []event.Transformer[E],
	aggProcessor aggregate.TransactionalAggregateProcessor[TX, TID, E, R],
	opts ...aggregate.ESRepoOption,
) (*aggregate.TransactionalRepository[TX, TID, E, R], error) {
	return aggregate.NewTransactionalRepositoryWithTransactor(
		transactor,
		txLog,
		createRoot,
		transformers,
		aggProcessor,
		opts...)
}
