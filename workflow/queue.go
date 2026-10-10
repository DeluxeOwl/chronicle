package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid/v5"
)

// TaskQueue is the abstraction that decouples workflow scheduling from
// the Runner. Built-in implementations use memory, transactional SQL tables,
// or SQLite tables populated by an asynchronous projection.
//
// ID-only acknowledgments cannot identify overlapping claims. Concurrent
// workers prefer ClaimedTaskQueue when the implementation supports it.
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

// ClaimReceipt identifies one claim, not a worker or workflow instance.
// A new receipt is required whenever an expired task is reclaimed.
type ClaimReceipt string

// ClaimedTask binds a task to its current ownership receipt.
type ClaimedTask struct {
	QueuedTask
	Receipt ClaimReceipt
}

// ErrStaleClaim means the receipt expired, was replaced, or was acknowledged.
// The rejected operation must not affect the current task or its successor.
var ErrStaleClaim = errors.New("task claim is stale or expired")

// ClaimedTaskQueue optionally adds receipt-aware ownership to TaskQueue.
// Claims remain recoverable after expiry. All acknowledgment and extension
// operations reject expired or stale receipts with ErrStaleClaim.
// Re-enqueueing a claimed instance must preserve its follow-up task when the
// original claim is acknowledged. These guarantees do not fence external
// workflow effects or make their execution exactly once.
type ClaimedTaskQueue interface {
	TaskQueue
	PollClaim(ctx context.Context) (*ClaimedTask, error)
	CompleteClaim(ctx context.Context, task ClaimedTask) error
	FailClaim(ctx context.Context, task ClaimedTask) error
	ExtendClaim(ctx context.Context, task ClaimedTask, lease time.Duration) error
}

// ClaimedTaskReleaser optionally makes an unfinished claim runnable again.
// A pending successor takes precedence over the interrupted task. Without
// this interface, workers must leave claims to expire rather than delete them.
type ClaimedTaskReleaser interface {
	ReleaseClaim(ctx context.Context, task ClaimedTask, runAfter time.Time) error
}

const defaultQueueLeaseDuration = 30 * time.Second

func newClaimReceipt() ClaimReceipt {
	return ClaimReceipt(uuid.Must(uuid.NewV4()).String())
}

func validateLease(lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("queue lease must be positive: %s", lease)
	}
	return nil
}

func laterDeadline(current, proposed time.Time) time.Time {
	if proposed.After(current) {
		return proposed
	}
	return current
}

// QueuedTask represents a unit of work on the queue.
type QueuedTask struct {
	InstanceID   InstanceID
	WorkflowName string
	RunAfter     time.Time `exhaustruct:"optional"` // zero value = immediately ready
}
