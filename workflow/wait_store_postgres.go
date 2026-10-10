package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// postgresWaitStore publishes payloads and wakes waiters in one transaction.
// Registration is performed by PostgresQueue.Process, under the same lock.
type postgresWaitStore struct {
	db                 *sql.DB
	transactionTimeout time.Duration
}

func newPostgresWaitStore(db *sql.DB, timeout time.Duration) *postgresWaitStore {
	return &postgresWaitStore{db: db, transactionTimeout: timeout}
}

func (*postgresWaitStore) Register(context.Context, InstanceID, string, string) {}

func (p *postgresWaitStore) Publish(
	ctx context.Context,
	name string,
	payload json.RawMessage,
	_ time.Time,
) ([]waitingWorkflow, error) {
	var waiters []waitingWorkflow
	err := runPostgresWorkflowTx(
		ctx,
		p.db,
		p.transactionTimeout,
		func(ctx context.Context, tx *sql.Tx) error {
			// The shared lock precedes all task locks and waiter changes. Registration
			// and publication cannot both overlook each other, including an empty set.
			if err := lockPostgresWaitsTx(ctx, tx); err != nil {
				return err
			}
			// Publication time is accepted-storage time, sampled after the wait lock.
			// It is not the physical commit time. Workflow deadline clocks must agree
			// with the database clock, rather than relying on a publisher's host clock.
			if _, err := tx.ExecContext(ctx, `
			INSERT INTO workflow_published_events (event_name, payload, published_at_ns)
			VALUES ($1, $2, `+postgresClockNS+`) ON CONFLICT (event_name) DO NOTHING
		`, name, string(payload)); err != nil {
				return fmt.Errorf("postgres store publication: %w", err)
			}
			txWaiters, err := postgresPublicationWaiters(ctx, tx, name)
			if err != nil {
				return err
			}
			// Lock task rows in the same deterministic order for every publication.
			// Repeated publications retain the first payload and repair outstanding wakes.
			for _, waiter := range txWaiters {
				if _, err := tx.ExecContext(ctx, enqueuePostgresTaskSQL, string(waiter.InstanceID), waiter.WorkflowName, int64(0)); err != nil {
					return fmt.Errorf("postgres schedule publication: %w", err)
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_waiting_events WHERE event_name = $1`, name); err != nil {
				return fmt.Errorf("postgres delete publication waiters: %w", err)
			}
			waiters = txWaiters
			return nil
		},
	)
	if err != nil {
		return nil, err
	}
	return waiters, nil
}

func postgresPublicationWaiters(
	ctx context.Context,
	tx *sql.Tx,
	name string,
) ([]waitingWorkflow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT instance_id, workflow_name FROM workflow_waiting_events
		WHERE event_name = $1 ORDER BY instance_id
	`, name)
	if err != nil {
		return nil, fmt.Errorf("postgres read publication waiters: %w", err)
	}
	defer rows.Close()
	waiters := make([]waitingWorkflow, 0)
	for rows.Next() {
		var waiter waitingWorkflow
		if err := rows.Scan(&waiter.InstanceID, &waiter.WorkflowName); err != nil {
			return nil, fmt.Errorf("postgres read publication waiter: %w", err)
		}
		waiters = append(waiters, waiter)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres read publication waiters: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("postgres close publication waiters: %w", err)
	}
	return waiters, nil
}

func (p *postgresWaitStore) GetEvent(
	ctx context.Context,
	_ InstanceID,
	name string,
) (publishedEvent, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.transactionTimeout)
	defer cancel()
	var payload string
	var ns int64
	err := p.db.QueryRowContext(ctx, `
		SELECT payload, published_at_ns FROM workflow_published_events WHERE event_name = $1
	`, name).Scan(&payload, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return publishedEvent{Payload: nil, PublishedAt: time.Time{}}, false, nil
	}
	if err != nil {
		return publishedEvent{
				Payload:     nil,
				PublishedAt: time.Time{},
			}, false, fmt.Errorf(
				"postgres read publication: %w",
				err,
			)
	}
	var publishedAt time.Time
	if ns != 0 {
		publishedAt = time.Unix(0, ns)
	}
	return publishedEvent{Payload: json.RawMessage(payload), PublishedAt: publishedAt}, true, nil
}

var _ eventWaitStore = (*postgresWaitStore)(nil)
