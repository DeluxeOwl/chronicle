package chronicle

import (
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
)

// Registries.
func NewEventRegistry[E event.Any]() *event.EventRegistry[E] {
	return event.NewRegistry[E]()
}

func NewAnyEventRegistry() *event.EventRegistry[event.Any] {
	return event.NewRegistry[event.Any]()
}

// Repositories.
func NewEventSourcedRepository[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E]](
	eventlog event.Log,
	createRoot func() R,
	transformers []event.Transformer[E],
	opts ...aggregate.ESRepoOption,
) (*aggregate.ESRepo[TID, E, R], error) {
	return aggregate.NewESRepo(eventlog, createRoot, nil, opts...)
}

func NewEventSourcedRepositoryWithSnapshots[TID aggregate.ID, E event.Any, R aggregate.Root[TID, E], TS aggregate.Snapshot[TID]](
	esRepo aggregate.Repository[TID, E, R],
	snapstore aggregate.SnapshotStore[TID, TS],
	snapshotter aggregate.Snapshotter[TID, E, R, TS],
	snapstrategy aggregate.SnapshotStrategy[TID, E, R],
	opts ...aggregate.ESRepoWithSnapshotsOption,
) (*aggregate.ESRepoWithSnapshots[TID, E, R, TS], error) {
	return aggregate.NewESRepoWithSnapshots(
		esRepo,
		snapstore,
		snapshotter,
		snapstrategy,
		opts...)
}

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
