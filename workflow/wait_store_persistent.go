package workflow

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// persistentWaitStore is a SQL-backed implementation of eventWaitStore.
// It uses workflow_waiting_events and workflow_emitted_events tables to
// durably track event waiting state across process restarts.
//
// The transactional writes (insert/delete waiters during event processing)
// are handled by SyncQueue.Process() within the same DB transaction as
// event writes. This store handles the non-transactional operations:
// emitting events (from EmitEvent) and checking for emitted events
// (from AwaitEvent replay).
//
// This enables distributed AwaitEvent/EmitEvent: an event emitted from
// one process can wake workflows running on another, because both read
// from the same SQL tables.
type persistentWaitStore struct {
	db *sql.DB
}

func newPersistentWaitStore(db *sql.DB) (*persistentWaitStore, error) {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_waiting_events (
			instance_id TEXT NOT NULL,
			event_name TEXT NOT NULL,
			workflow_name TEXT NOT NULL,
			step_index INTEGER NOT NULL DEFAULT 0,
			deadline_ns INTEGER,
			PRIMARY KEY (instance_id, event_name)
		)
	`); err != nil {
		return nil, fmt.Errorf("create waiting_events table: %w", err)
	}

	// Index for efficient lookup by event name (used by Emit).
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_workflow_waiting_events_name
		ON workflow_waiting_events (event_name)
	`); err != nil {
		return nil, fmt.Errorf("create waiting_events index: %w", err)
	}

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS workflow_emitted_events (
			event_name TEXT PRIMARY KEY,
			payload TEXT NOT NULL
		)
	`); err != nil {
		return nil, fmt.Errorf("create emitted_events table: %w", err)
	}

	return &persistentWaitStore{db: db}, nil
}

// Register is a no-op for the persistent store.
// The SyncQueue.Process() handles inserting waiters into workflow_waiting_events
// atomically during the event-save transaction. It also detects pre-emitted
// events and creates immediate wake-up tasks.
func (p *persistentWaitStore) Register(_ InstanceID, _ string, _ string) {
	// No-op: handled atomically by SyncQueue.Process() for workflowWaiting events.
}

// Emit stores an event payload (first-write-wins) and returns any workflows
// that were waiting for this event. Matched waiters are removed from the
// waiting table. All operations run in a single transaction to prevent
// missed wake-ups.
func (p *persistentWaitStore) Emit(eventName string, payload json.RawMessage) []waitingWorkflow {
	tx, err := p.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback() //nolint:errcheck

	// First-write-wins: INSERT OR IGNORE
	result, err := tx.Exec(`
		INSERT OR IGNORE INTO workflow_emitted_events (event_name, payload)
		VALUES (?, ?)
	`, eventName, string(payload))
	if err != nil {
		return nil
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		// Event was already emitted — first-write-wins, no new waiters to wake.
		return nil
	}

	// Find all workflows waiting for this event
	rows, err := tx.Query(`
		SELECT instance_id, workflow_name
		FROM workflow_waiting_events
		WHERE event_name = ?
	`, eventName)
	if err != nil {
		return nil
	}

	var waiters []waitingWorkflow
	for rows.Next() {
		var instanceID string
		var workflowName string
		if err := rows.Scan(&instanceID, &workflowName); err != nil {
			continue
		}
		waiters = append(waiters, waitingWorkflow{
			InstanceID:   InstanceID(instanceID),
			WorkflowName: workflowName,
		})
	}
	rows.Close()

	// Remove matched waiters
	if len(waiters) > 0 {
		_, _ = tx.Exec(`
			DELETE FROM workflow_waiting_events WHERE event_name = ?
		`, eventName)
	}

	if err := tx.Commit(); err != nil {
		return nil
	}

	return waiters
}

// GetEvent checks if an event has been emitted by looking up the
// workflow_emitted_events table. The instanceID is not used for the
// query — emitted events are global (first-write-wins).
func (p *persistentWaitStore) GetEvent(_ InstanceID, eventName string) (json.RawMessage, bool) {
	var payload string
	err := p.db.QueryRow(`
		SELECT payload FROM workflow_emitted_events WHERE event_name = ?
	`, eventName).Scan(&payload)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(payload), true
}

// Compile-time interface check.
var _ eventWaitStore = (*persistentWaitStore)(nil)
