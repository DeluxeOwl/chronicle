package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/DeluxeOwl/chronicle"
	"github.com/DeluxeOwl/chronicle/aggregate"
	"github.com/DeluxeOwl/chronicle/event"
	"github.com/DeluxeOwl/chronicle/eventlog"
)

// WithPostgresQueueOpts forwards queue options to NewPostgresRunnerWithSyncQueue.
// Other runner constructors ignore these options.
func WithPostgresQueueOpts(opts ...PostgresQueueOption) RunnerOption {
	return func(r *Runner) { r.postgresQueueOpts = append(r.postgresQueueOpts, opts...) }
}

// NewPostgresRunnerWithSyncQueue wires a shared PostgreSQL queue, persistent
// event waits, and transactional event repository for workers on separate hosts.
// Queue leases use database time; WithNowFunc affects workflow timestamps only.
// Storage transactions use read-committed isolation and the queue's timeout.
// Activity execution is not limited by the storage transaction timeout.
func NewPostgresRunnerWithSyncQueue(
	ctx context.Context,
	db *sql.DB,
	opts ...RunnerOption,
) (*Runner, error) {
	if db == nil {
		return nil, errors.New("postgres workflow database cannot be nil")
	}
	// Options are applied once, before constructing the durable components.
	//nolint:exhaustruct // Repository, registry, and queue are constructed below.
	r := &Runner{
		logger:    slog.Default(),
		nowFunc:   time.Now,
		workflows: make(map[string]executableWorkflow),
	}
	for _, opt := range opts {
		if opt == nil {
			return nil, errors.New("runner option cannot be nil")
		}
		opt(r)
	}
	if r.nowFunc == nil {
		return nil, errors.New("runner clock cannot be nil")
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	queue, err := NewPostgresQueue(ctx, db, r.postgresQueueOpts...)
	if err != nil {
		return nil, fmt.Errorf("create postgres workflow queue: %w", err)
	}
	log, err := eventlog.NewPostgres(db)
	if err != nil {
		return nil, fmt.Errorf("create postgres workflow event log: %w", err)
	}
	// A fresh registry gives the repository a typed view without global state.
	registry := event.NewRegistry[event.Any]()
	if err := registry.RegisterEvents(&eventFuncWrapper{funcs: NewEmpty().EventFuncs()}); err != nil {
		return nil, fmt.Errorf("register workflow events: %w", err)
	}
	transactor := &postgresWorkflowTransactor{db: db, timeout: queue.transactionTimeout}
	repo, err := chronicle.NewTransactionalRepositoryWithTransactor(
		transactor, log, NewEmpty, nil, queue,
		aggregate.DontRegisterRoot(), aggregate.AnyEventRegistry(registry),
	)
	if err != nil {
		return nil, fmt.Errorf("create postgres workflow repository: %w", err)
	}
	r.repo = repo
	r.eventRegistry = registry
	r.queue = queue
	r.waitStore = newPostgresWaitStore(db, queue.transactionTimeout)
	r.tasksInTx = true
	return r, nil
}
