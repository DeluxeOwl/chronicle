package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// persistentWaitStore keeps publication and task wake-up in one transaction.
// Wait registration is part of SyncQueue's event-save transaction.
type persistentWaitStore struct {
	db      *sql.DB
	nowFunc func() time.Time
}

func newPersistentWaitStore(db *sql.DB, nowFunc func() time.Time) *persistentWaitStore {
	return &persistentWaitStore{db: db, nowFunc: nowFunc}
}

func (*persistentWaitStore) Register(context.Context, InstanceID, string, string) {}

func (p *persistentWaitStore) Publish(
	ctx context.Context,
	name string,
	payload json.RawMessage,
	_ time.Time,
) ([]waitingWorkflow, error) {
	waiters := make([]waitingWorkflow, 0)
	err := withSQLiteWriteTx(ctx, p.db, func(tx *sql.Tx) error {
		// Accepted-storage time is sampled after acquiring the write lock, not
		// before waiting for a connection or a concurrent timeout transaction.
		// This preserves the runner's custom clock, not physical commit time.
		publishedAt := p.nowFunc()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO workflow_published_events(event_name,payload,published_at_ns) VALUES(?,?,?)`, name, string(payload), queueTimeNS(publishedAt)); err != nil {
			return fmt.Errorf("store event publication: %w", err)
		}
		// Repeated publications repair wake-ups without replacing the original
		// payload or the timestamp accepted by the first successful write.
		rows, err := tx.QueryContext(
			ctx,
			`SELECT instance_id,workflow_name FROM workflow_waiting_events WHERE event_name=?`,
			name,
		)
		if err != nil {
			return fmt.Errorf("read event waiters: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var waiter waitingWorkflow
			if err := rows.Scan(&waiter.InstanceID, &waiter.WorkflowName); err != nil {
				return fmt.Errorf("read event waiter: %w", err)
			}
			waiters = append(waiters, waiter)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read event waiters: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close event waiters: %w", err)
		}
		for _, waiter := range waiters {
			if _, err := tx.ExecContext(ctx, enqueueReadyTaskSQL, string(waiter.InstanceID), waiter.WorkflowName, int64(0)); err != nil {
				return fmt.Errorf("schedule published event: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflow_waiting_events WHERE event_name=?`, name); err != nil {
			return fmt.Errorf("remove event waiters: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("publish event transaction: %w", err)
	}
	return waiters, nil
}

func (p *persistentWaitStore) GetEvent(
	ctx context.Context,
	_ InstanceID,
	name string,
) (publishedEvent, bool, error) {
	var payload string
	var ns int64
	err := p.db.QueryRowContext(ctx, `SELECT payload,published_at_ns FROM workflow_published_events WHERE event_name=?`, name).
		Scan(&payload, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return publishedEvent{Payload: nil, PublishedAt: time.Time{}}, false, nil
	}
	if err != nil {
		return publishedEvent{
				Payload:     nil,
				PublishedAt: time.Time{},
			}, false, fmt.Errorf(
				"read event publication: %w",
				err,
			)
	}
	var publishedAt time.Time
	if ns != 0 {
		publishedAt = time.Unix(0, ns)
	}
	return publishedEvent{Payload: json.RawMessage(payload), PublishedAt: publishedAt}, true, nil
}

var _ eventWaitStore = (*persistentWaitStore)(nil)
