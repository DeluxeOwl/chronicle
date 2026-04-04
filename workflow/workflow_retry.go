package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrWorkflowRetrying is returned by Run when the workflow has failed
// and has been re-enqueued for retry with backoff.
var ErrWorkflowRetrying = errors.New("workflow is retrying")

// RetryStrategy configures how failed workflows are retried.
type RetryStrategy struct {
	// MaxAttempts is the maximum number of times a workflow can be executed.
	// This includes the initial attempt. 0 means use the runner's default (5).
	MaxAttempts int

	// BaseDelay is the initial backoff delay.
	BaseDelay time.Duration

	// Factor is the multiplier applied to the delay after each failure.
	// For exponential backoff, use 2.0.
	Factor float64

	// MaxDelay is the maximum delay between retries.
	MaxDelay time.Duration
}

// DefaultRetryStrategy returns a sensible default retry strategy:
// 5 attempts, exponential backoff starting at 1s, factor of 2, max 5 minutes.
func DefaultRetryStrategy() RetryStrategy {
	return RetryStrategy{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Second,
		Factor:      2.0,
		MaxDelay:    5 * time.Minute,
	}
}

// nextDelay computes the delay for the given attempt (0-indexed).
// attempt=0 is after the first failure, attempt=1 is after the second, etc.
func (rs RetryStrategy) nextDelay(attempt int) time.Duration {
	if rs.Factor <= 0 {
		return rs.BaseDelay
	}
	delay := float64(rs.BaseDelay) * math.Pow(rs.Factor, float64(attempt))
	if rs.MaxDelay > 0 && time.Duration(delay) > rs.MaxDelay {
		return rs.MaxDelay
	}
	return time.Duration(delay)
}

// retryState is persisted in the event log as a workflowRetried event's data.
// It tracks how many attempts have been made so far.
type retryState struct {
	Attempt       int           `json:"attempt"`       // 1-indexed current attempt number
	NextRunAfter  time.Time     `json:"nextRunAfter"`  // When the next retry is scheduled
	BackoffDelay  time.Duration `json:"backoffDelay"`  // The delay used for this retry
	PreviousError string        `json:"previousError"` // The error from the previous attempt
}

// workflowRetried records that a failed workflow has been scheduled for retry.
type workflowRetried struct {
	Attempt       int             `json:"attempt"`
	NextRunAfter  time.Time       `json:"nextRunAfter"`
	BackoffDelay  time.Duration   `json:"backoffDelay"`
	PreviousError string          `json:"previousError"`
	RetryStrategy json.RawMessage `json:"retryStrategy"`
}

func (*workflowRetried) EventName() string { return "workflow/retried" }
func (*workflowRetried) isWorkflowEvent()  {}

// isRetryError checks if an error is ErrWorkflowRetrying.
func isRetryError(err error) bool {
	return errors.Is(err, ErrWorkflowRetrying)
}

// scheduleRetry attempts to schedule a retry for a failed workflow.
// It returns true if a retry was scheduled, false if max attempts exhausted.
func (r *Runner) scheduleRetry(
	ctx *Context,
	instance *WorkflowInstance,
	workflowErr error,
) (bool, error) {
	strategy := instance.retryStrategy
	if strategy == nil {
		return false, nil // no retry configured
	}

	if strategy.MaxAttempts > 0 && instance.attempt >= strategy.MaxAttempts {
		return false, nil // exhausted
	}

	now := r.now()
	nextAttempt := instance.attempt + 1
	delay := strategy.nextDelay(instance.attempt - 1) // attempt is 1-indexed, nextDelay wants 0-indexed failures
	runAfter := now.Add(delay)

	retryStrategyJSON, err := json.Marshal(strategy)
	if err != nil {
		return false, fmt.Errorf("marshal retry strategy: %w", err)
	}

	if err := instance.recordThat(&workflowRetried{
		Attempt:       nextAttempt,
		NextRunAfter:  runAfter,
		BackoffDelay:  delay,
		PreviousError: workflowErr.Error(),
		RetryStrategy: retryStrategyJSON,
	}); err != nil {
		return false, fmt.Errorf("record workflow retried: %w", err)
	}

	if _, _, err := r.repo.Save(ctx.ctx, instance); err != nil {
		return false, fmt.Errorf("save workflow retried: %w", err)
	}

	// Enqueue for delayed re-execution
	if err := r.queue.Enqueue(ctx.ctx, QueuedTask{
		InstanceID:   ctx.instanceID,
		WorkflowName: instance.workflowName,
		RunAfter:     runAfter,
	}); err != nil {
		return false, fmt.Errorf("enqueue retry task: %w", err)
	}

	r.logger.Info(
		"workflow scheduled for retry",
		"instanceID", ctx.instanceID,
		"attempt", nextAttempt,
		"maxAttempts", strategy.MaxAttempts,
		"delay", delay,
		"runAfter", runAfter,
		"previousError", workflowErr.Error(),
	)

	return true, nil
}
