package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrWorkflowCancelled is returned by Run when the workflow has been cancelled.
var ErrWorkflowCancelled = errors.New("workflow cancelled")

// CancellationPolicy configures automatic cancellation rules for a workflow instance.
type CancellationPolicy struct {
	// MaxDuration cancels the workflow if it has been alive (since its started event)
	// longer than this duration. Zero means no max-duration limit.
	MaxDuration time.Duration

	// MaxDelay cancels the workflow if no new step has been completed for this
	// duration. Zero means no max-delay limit.
	MaxDelay time.Duration
}

// workflowCancelled records that a workflow has been cancelled.
type workflowCancelled struct {
	Reason      string    `json:"reason"`
	CancelledAt time.Time `json:"cancelledAt"`
}

func (*workflowCancelled) EventName() string { return "workflow/cancelled" }
func (*workflowCancelled) isWorkflowEvent()  {}

// WithCancellationPolicy configures a cancellation policy for the workflow instance.
func WithCancellationPolicy(cp CancellationPolicy) StartOption {
	return func(c *startConfig) {
		c.cancellationPolicy = &cp
	}
}

// CancelWorkflow cancels a running workflow instance. The workflow will be marked
// as cancelled and will not be picked up by workers again.
//
// Cancellation is idempotent: cancelling an already-cancelled, completed, or failed
// workflow is a no-op.
func CancelWorkflow(ctx context.Context, runner *Runner, instanceID InstanceID, reason string) error {
	instance, err := runner.repo.Get(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("load workflow instance for cancel: %w", err)
	}

	// Only cancel workflows that are still active.
	switch instance.status {
	case StatusCompleted, StatusFailed, StatusCancelled:
		return nil // Already terminal — no-op.
	}

	now := runner.now()

	if err := instance.recordThat(&workflowCancelled{
		Reason:      reason,
		CancelledAt: now,
	}); err != nil {
		return fmt.Errorf("record workflow cancelled: %w", err)
	}

	if _, _, err := runner.repo.Save(ctx, instance); err != nil {
		return fmt.Errorf("save cancelled workflow: %w", err)
	}

	runner.logger.Info(
		"workflow cancelled",
		"instanceID", instanceID,
		"reason", reason,
	)

	return nil
}

// checkCancellationPolicy checks if a workflow should be automatically cancelled
// based on its cancellation policy. Returns the reason for cancellation, or empty
// string if the workflow should continue.
func (r *Runner) checkCancellationPolicy(instance *WorkflowInstance) string {
	if instance.cancellationPolicy == nil {
		return ""
	}

	now := r.now()
	cp := instance.cancellationPolicy

	// Check MaxDuration: time since workflow started.
	if cp.MaxDuration > 0 && !instance.startedAt.IsZero() {
		if now.Sub(instance.startedAt) > cp.MaxDuration {
			return fmt.Sprintf("max duration exceeded (%s)", cp.MaxDuration)
		}
	}

	// Check MaxDelay: time since last checkpoint (step completion).
	if cp.MaxDelay > 0 && !instance.lastCheckpointAt.IsZero() {
		if now.Sub(instance.lastCheckpointAt) > cp.MaxDelay {
			return fmt.Sprintf("max delay exceeded (%s since last checkpoint)", cp.MaxDelay)
		}
	}

	return ""
}

// isCancelledError checks if an error is ErrWorkflowCancelled.
func isCancelledError(err error) bool {
	return errors.Is(err, ErrWorkflowCancelled)
}
