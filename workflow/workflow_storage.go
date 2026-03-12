package workflow

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Store is the primary interface for workflow metadata and scheduling.
// It provides both metadata storage and wake scheduling capabilities.
type Store interface {
	MetadataStore
	SchedulerStore
}

// MetadataStore handles workflow instance metadata operations.
type MetadataStore interface {
	// Create stores initial workflow metadata when a workflow is started.
	Create(ctx context.Context, info WorkflowInfo) error

	// Get retrieves workflow metadata by instance ID.
	Get(ctx context.Context, instanceID InstanceID) (*WorkflowInfo, error)

	// Update modifies workflow metadata. Only non-nil fields are updated.
	Update(ctx context.Context, instanceID InstanceID, updates Updates) error

	// List retrieves workflows matching the given options.
	List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error)

	// Count returns the total number of workflows matching the options.
	Count(ctx context.Context, opts ListOptions) (int, error)
}

// SchedulerStore handles wake scheduling for sleeping workflows.
type SchedulerStore interface {
	// ScheduleWake schedules a workflow to wake up at the specified time.
	ScheduleWake(ctx context.Context, instanceID InstanceID, wakeAt time.Time) error

	// GetDueWakes retrieves all workflows that should wake up before the given time.
	GetDueWakes(ctx context.Context, before time.Time) ([]InstanceID, error)

	// CompleteWake removes a wake schedule entry after the workflow has woken.
	CompleteWake(ctx context.Context, instanceID InstanceID) error
}

// Clock abstracts time for testing purposes.
type Clock interface {
	Now() time.Time
}

// realClock implements Clock using the system time.
type realClock struct{}

func (realClock) Now() time.Time {
	return time.Now()
}

// WorkflowInfo contains metadata about a workflow instance.
type WorkflowInfo struct {
	InstanceID   InstanceID
	WorkflowName string
	Status       Status
	CurrentStep  int
	Params       []byte
	Output       []byte
	Error        string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	SleepUntil   *time.Time
}

// Updates contains optional fields for updating workflow metadata.
// Only non-nil fields are applied during an update.
type Updates struct {
	Status      *Status
	CurrentStep *int
	Output      *[]byte
	Error       *string
	SleepUntil  *time.Time
}

// ListOptions provides filtering and pagination for workflow queries.
type ListOptions struct {
	WorkflowName string
	Status       Status
	Statuses     []Status // For filtering multiple statuses
	Limit        int
	Offset       int
	Since        *time.Time
	Until        *time.Time
}

// sqliteStore implements Store using SQLite.
type sqliteStore struct {
	db    *sql.DB
	clock Clock
}

// NewSQLiteStore creates a new SQLite-based workflow store.
func NewSQLiteStore(db *sql.DB) Store {
	return &sqliteStore{
		db:    db,
		clock: realClock{},
	}
}

// NewSQLiteStoreWithClock creates a store with a custom clock for testing.
func NewSQLiteStoreWithClock(db *sql.DB, clock Clock) Store {
	return &sqliteStore{
		db:    db,
		clock: clock,
	}
}

// Create stores initial workflow metadata.
func (s *sqliteStore) Create(ctx context.Context, info WorkflowInfo) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_instances 
		(instance_id, workflow_name, status, current_step, params, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, info.InstanceID, info.WorkflowName, info.Status, info.CurrentStep, info.Params, info.CreatedAt, info.UpdatedAt)

	if err != nil {
		return fmt.Errorf("create workflow metadata: %w", err)
	}

	return nil
}

// Get retrieves workflow metadata by instance ID.
func (s *sqliteStore) Get(ctx context.Context, instanceID InstanceID) (*WorkflowInfo, error) {
	var info WorkflowInfo
	var sleepUntil sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT instance_id, workflow_name, status, current_step, params, output, error, 
		       created_at, updated_at, sleep_until
		FROM workflow_instances
		WHERE instance_id = ?
	`, instanceID).Scan(
		&info.InstanceID,
		&info.WorkflowName,
		&info.Status,
		&info.CurrentStep,
		&info.Params,
		&info.Output,
		&info.Error,
		&info.CreatedAt,
		&info.UpdatedAt,
		&sleepUntil,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("workflow %s not found: %w", instanceID, err)
		}
		return nil, fmt.Errorf("get workflow metadata: %w", err)
	}

	if sleepUntil.Valid {
		info.SleepUntil = &sleepUntil.Time
	}

	return &info, nil
}

// Update modifies workflow metadata.
func (s *sqliteStore) Update(ctx context.Context, instanceID InstanceID, updates Updates) error {
	query := "UPDATE workflow_instances SET updated_at = ?"
	args := []interface{}{s.clock.Now()}

	if updates.Status != nil {
		query += ", status = ?"
		args = append(args, *updates.Status)
	}
	if updates.CurrentStep != nil {
		query += ", current_step = ?"
		args = append(args, *updates.CurrentStep)
	}
	if updates.Output != nil {
		query += ", output = ?"
		args = append(args, *updates.Output)
	}
	if updates.Error != nil {
		query += ", error = ?"
		args = append(args, *updates.Error)
	}
	if updates.SleepUntil != nil {
		query += ", sleep_until = ?"
		args = append(args, *updates.SleepUntil)
	}

	query += " WHERE instance_id = ?"
	args = append(args, instanceID)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update workflow metadata: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("workflow %s not found", instanceID)
	}

	return nil
}

// List retrieves workflows matching the given options.
func (s *sqliteStore) List(ctx context.Context, opts ListOptions) ([]WorkflowInfo, error) {
	query := `
		SELECT instance_id, workflow_name, status, current_step, params, output, error, 
		       created_at, updated_at, sleep_until
		FROM workflow_instances
		WHERE 1=1
	`
	args := []interface{}{}

	if opts.WorkflowName != "" {
		query += " AND workflow_name = ?"
		args = append(args, opts.WorkflowName)
	}

	if opts.Status != "" {
		query += " AND status = ?"
		args = append(args, opts.Status)
	}

	if len(opts.Statuses) > 0 {
		query += " AND status IN ("
		for i, status := range opts.Statuses {
			if i > 0 {
				query += ", "
			}
			query += "?"
			args = append(args, status)
		}
		query += ")"
	}

	if opts.Since != nil {
		query += " AND created_at >= ?"
		args = append(args, *opts.Since)
	}

	if opts.Until != nil {
		query += " AND created_at <= ?"
		args = append(args, *opts.Until)
	}

	query += " ORDER BY created_at DESC"

	if opts.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, opts.Limit)
	}

	if opts.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, opts.Offset)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list workflows: %w", err)
	}
	defer rows.Close()

	var workflows []WorkflowInfo

	for rows.Next() {
		var info WorkflowInfo
		var sleepUntil sql.NullTime

		err := rows.Scan(
			&info.InstanceID,
			&info.WorkflowName,
			&info.Status,
			&info.CurrentStep,
			&info.Params,
			&info.Output,
			&info.Error,
			&info.CreatedAt,
			&info.UpdatedAt,
			&sleepUntil,
		)
		if err != nil {
			return nil, fmt.Errorf("scan workflow row: %w", err)
		}

		if sleepUntil.Valid {
			info.SleepUntil = &sleepUntil.Time
		}

		workflows = append(workflows, info)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow rows: %w", err)
	}

	return workflows, nil
}

// Count returns the total number of workflows matching the options.
func (s *sqliteStore) Count(ctx context.Context, opts ListOptions) (int, error) {
	query := "SELECT COUNT(*) FROM workflow_instances WHERE 1=1"
	args := []interface{}{}

	if opts.WorkflowName != "" {
		query += " AND workflow_name = ?"
		args = append(args, opts.WorkflowName)
	}

	if opts.Status != "" {
		query += " AND status = ?"
		args = append(args, opts.Status)
	}

	if len(opts.Statuses) > 0 {
		query += " AND status IN ("
		for i, status := range opts.Statuses {
			if i > 0 {
				query += ", "
			}
			query += "?"
			args = append(args, status)
		}
		query += ")"
	}

	if opts.Since != nil {
		query += " AND created_at >= ?"
		args = append(args, *opts.Since)
	}

	if opts.Until != nil {
		query += " AND created_at <= ?"
		args = append(args, *opts.Until)
	}

	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count workflows: %w", err)
	}

	return count, nil
}

// ScheduleWake schedules a workflow to wake up at the specified time.
func (s *sqliteStore) ScheduleWake(ctx context.Context, instanceID InstanceID, wakeAt time.Time) error {
	// Start a transaction to ensure atomicity
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Insert or update wake schedule
	_, err = tx.ExecContext(ctx, `
		INSERT INTO workflow_wake_schedule (instance_id, wake_at, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET 
			wake_at = excluded.wake_at,
			created_at = excluded.created_at
	`, instanceID, wakeAt, s.clock.Now())
	if err != nil {
		return fmt.Errorf("schedule wake: %w", err)
	}

	// Update workflow status to sleeping
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances 
		SET status = 'sleeping', sleep_until = ?, updated_at = ?
		WHERE instance_id = ?
	`, wakeAt, s.clock.Now(), instanceID)
	if err != nil {
		return fmt.Errorf("update workflow status to sleeping: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit wake schedule: %w", err)
	}

	return nil
}

// GetDueWakes retrieves all workflows that should wake up before the given time.
func (s *sqliteStore) GetDueWakes(ctx context.Context, before time.Time) ([]InstanceID, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT instance_id FROM workflow_wake_schedule
		WHERE wake_at <= ?
		ORDER BY wake_at ASC
	`, before)
	if err != nil {
		return nil, fmt.Errorf("get due wakes: %w", err)
	}
	defer rows.Close()

	var instanceIDs []InstanceID
	for rows.Next() {
		var id InstanceID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan wake row: %w", err)
		}
		instanceIDs = append(instanceIDs, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate wake rows: %w", err)
	}

	return instanceIDs, nil
}

// CompleteWake removes a wake schedule entry after the workflow has woken.
func (s *sqliteStore) CompleteWake(ctx context.Context, instanceID InstanceID) error {
	// Start a transaction
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Remove from wake schedule
	_, err = tx.ExecContext(ctx, `
		DELETE FROM workflow_wake_schedule WHERE instance_id = ?
	`, instanceID)
	if err != nil {
		return fmt.Errorf("complete wake: %w", err)
	}

	// Clear sleep_until and update status to running
	_, err = tx.ExecContext(ctx, `
		UPDATE workflow_instances 
		SET status = 'running', sleep_until = NULL, updated_at = ?
		WHERE instance_id = ?
	`, s.clock.Now(), instanceID)
	if err != nil {
		return fmt.Errorf("update workflow status after wake: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit complete wake: %w", err)
	}

	return nil
}

// MigrateSQLite creates the necessary tables for the workflow store.
// Call this function when initializing your application.
func MigrateSQLite(db *sql.DB) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS workflow_instances (
			instance_id     TEXT PRIMARY KEY,
			workflow_name   TEXT NOT NULL,
			status          TEXT NOT NULL,
			current_step    INTEGER DEFAULT 0,
			params          BLOB,
			output          BLOB,
			error           TEXT,
			created_at      TIMESTAMP NOT NULL,
			updated_at      TIMESTAMP NOT NULL,
			sleep_until     TIMESTAMP
		);`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_name ON workflow_instances(workflow_name);`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_status ON workflow_instances(status);`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_sleep ON workflow_instances(sleep_until) WHERE sleep_until IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_workflow_created ON workflow_instances(created_at);`,
		`CREATE TABLE IF NOT EXISTS workflow_wake_schedule (
			instance_id TEXT PRIMARY KEY,
			wake_at     TIMESTAMP NOT NULL,
			created_at  TIMESTAMP NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_wake_at ON workflow_wake_schedule(wake_at);`,
	}

	for _, migration := range migrations {
		if _, err := db.Exec(migration); err != nil {
			return fmt.Errorf("migration failed: %s: %w", migration[:50], err)
		}
	}

	return nil
}