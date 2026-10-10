package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DeluxeOwl/chronicle/aggregate"
)

var errEventWaitDecisionChanged = errors.New(
	"event wait timeout superseded by an on-time publication",
)

func eventPublicationOnTime(publishedAt, deadline time.Time) bool {
	return deadline.IsZero() || publishedAt.IsZero() || publishedAt.Before(deadline)
}

// The append has already acquired SQLite's write lock. A timeout must validate
// publication in that same transaction, not against a preceding unlocked read.
func validateSQLiteWaitTimeoutsTx(
	ctx context.Context,
	tx *sql.Tx,
	events aggregate.CommittedEvents[WorkflowEvent],
) error {
	waits, err := eventWaitTimeouts(events)
	if err != nil {
		return err
	}
	return validateWaitTimeoutsTx(
		ctx,
		tx,
		waits,
		`SELECT published_at_ns FROM workflow_published_events WHERE event_name = ?`,
	)
}

// Call before queue claim fencing: publishers acquire the same advisory lock
// before task rows. Standalone timed-out wait checkpoints need this lock too.
func validatePostgresWaitTimeoutsTx(
	ctx context.Context,
	tx *sql.Tx,
	events aggregate.CommittedEvents[WorkflowEvent],
) error {
	waits, err := eventWaitTimeouts(events)
	if err != nil || len(waits) == 0 {
		return err
	}
	if err := lockPostgresWaitsTx(ctx, tx); err != nil {
		return err
	}
	return validateWaitTimeoutsTx(
		ctx,
		tx,
		waits,
		`SELECT published_at_ns FROM workflow_published_events WHERE event_name = $1`,
	)
}

func eventWaitTimeouts(
	events aggregate.CommittedEvents[WorkflowEvent],
) ([]waitForEventResult, error) {
	var waits []waitForEventResult
	for evt := range events.All() {
		completed, ok := evt.(*stepCompleted)
		if !ok || completed.Kind != stepKindWait {
			continue
		}
		var wait waitForEventResult
		if err := json.Unmarshal(completed.Result, &wait); err != nil {
			return nil, infrastructureError(fmt.Errorf("decode event wait decision: %w", err))
		}
		if wait.TimedOut {
			waits = append(waits, wait)
		}
	}
	return waits, nil
}

func validateWaitTimeoutsTx(
	ctx context.Context,
	tx *sql.Tx,
	waits []waitForEventResult,
	query string,
) error {
	for _, wait := range waits {
		var ns int64
		err := tx.QueryRowContext(ctx, query, wait.EventName).Scan(&ns)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return infrastructureError(fmt.Errorf("validate event wait timeout: %w", err))
		}
		if ns == 0 || eventPublicationOnTime(time.Unix(0, ns), wait.Deadline) {
			// Rejecting the processor rolls back the append. Run returns this as
			// infrastructure failure without recording failure or spending retries.
			return infrastructureError(errEventWaitDecisionChanged)
		}
	}
	return nil
}
