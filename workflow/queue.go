package workflow

import (
	"context"
	"time"
)

// TaskQueue is the abstraction that decouples workflow scheduling from
// the Runner.  Implementations range from a simple in-memory list
// (single-process / testing) to a Postgres-backed table populated by a
// SyncProjection (distributed, transactional) or an AsyncProjection
// (distributed, eventually-consistent).
type TaskQueue interface {
	// Enqueue adds a task to the queue.
	// If task.RunAfter is non-zero, the task is not eligible for polling
	// until that time has passed (used by Sleep to schedule wake-ups).
	Enqueue(ctx context.Context, task QueuedTask) error

	// Poll returns the next ready task (RunAfter <= now, not yet claimed)
	// and removes it from the ready set.
	// Returns nil, nil when no tasks are ready.
	Poll(ctx context.Context) (*QueuedTask, error)

	// Complete marks a task as successfully finished.
	Complete(ctx context.Context, instanceID InstanceID) error

	// Fail marks a task as failed.
	Fail(ctx context.Context, instanceID InstanceID) error

	// ExtendLease extends the claim timeout for a running task.
	// This signals that the worker is still making progress on a long-running step.
	// Implementations that don't support leases may no-op.
	ExtendLease(ctx context.Context, instanceID InstanceID, lease time.Duration) error
}

// QueuedTask represents a unit of work on the queue.
type QueuedTask struct {
	InstanceID   InstanceID
	WorkflowName string
	RunAfter     time.Time // zero value = immediately ready
}
