package workflow

import (
	"database/sql"
	"encoding/json"
)

// persistentWaitStore is a SQL-backed implementation of eventWaitStore.
// It uses workflow_waiting_events and workflow_published_events tables to
// durably track event waiting state across process restarts.
//
// The transactional writes (insert/delete waiters during event processing)
// are handled by SyncQueue.Process() within the same DB transaction as
// event writes. This store handles the non-transactional operations:
// publishing events (from PublishEvent) and checking for published events
// (from WaitForEvent replay).
//
// This enables distributed WaitForEvent/PublishEvent: an event published from
// one process can wake workflows running on another, because both read
// from the same SQL tables.
type persistentWaitStore struct {
	db *sql.DB
}

// newPersistentWaitStore creates a persistent wait store.
// The required tables (workflow_waiting_events, workflow_published_events) must
// already exist — they are created by NewSyncQueue / NewAsyncQueue.
func newPersistentWaitStore(db *sql.DB) *persistentWaitStore {
	return &persistentWaitStore{db: db}
}

// Register is a no-op for the persistent store.
// The SyncQueue.Process() handles inserting waiters into workflow_waiting_events
// atomically during the event-save transaction. It also detects pre-published
// events and creates immediate wake-up tasks.
func (p *persistentWaitStore) Register(_ InstanceID, _ string, _ string) {
	// No-op: handled atomically by SyncQueue.Process() for workflowWaiting events.
}

// Publish stores an event payload (first-write-wins) and returns any workflows
// that were waiting for this event. Matched waiters are removed from the
// waiting table. All operations run in a single transaction to prevent
// missed wake-ups.
func (p *persistentWaitStore) Publish(eventName string, payload json.RawMessage) []waitingWorkflow {
	tx, err := p.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback() //nolint:errcheck // not needed.

	// First-write-wins: INSERT OR IGNORE
	result, err := tx.Exec(`
		INSERT OR IGNORE INTO workflow_published_events (event_name, payload)
		VALUES (?, ?)
	`, eventName, string(payload))
	if err != nil {
		return nil
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		// Event was already published — first-write-wins, no new waiters to wake.
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
	if rows.Err() != nil {
		return nil
	}
	defer rows.Close()

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

// GetEvent checks if an event has been published by looking up the
// workflow_published_events table. The instanceID is not used for the
// query — published events are global (first-write-wins).
func (p *persistentWaitStore) GetEvent(_ InstanceID, eventName string) (json.RawMessage, bool) {
	var payload string
	err := p.db.QueryRow(`
		SELECT payload FROM workflow_published_events WHERE event_name = ?
	`, eventName).Scan(&payload)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(payload), true
}

// Compile-time interface check.
var _ eventWaitStore = (*persistentWaitStore)(nil)
