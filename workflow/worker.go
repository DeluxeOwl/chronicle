package workflow

import (
	"context"
	"fmt"
	"time"
)

const (
	DefaultPollInterval       = 200 * time.Millisecond
	DefaultLeaseExtendInterval = 10 * time.Second
)

// WorkerOptions configures a worker started via RunWorker.
type WorkerOptions struct {
	// PollInterval is how long the worker waits between queue polls
	// when no tasks are ready. Defaults to 200ms.
	PollInterval time.Duration

	// LeaseExtendInterval is how often the worker automatically extends
	// the task lease while a workflow execution is in progress. This
	// prevents other workers from reclaiming the task if a step takes
	// longer than the queue's lease duration.
	//
	// Defaults to 10s. Set to 0 to disable automatic lease extension
	// (you must then call Heartbeat manually from long-running steps).
	LeaseExtendInterval time.Duration
}

func (o WorkerOptions) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return o.PollInterval
}

func (o WorkerOptions) leaseExtendInterval() time.Duration {
	// Explicitly set to 0 disables auto-extension.
	if o.LeaseExtendInterval < 0 {
		return DefaultLeaseExtendInterval
	}
	if o.LeaseExtendInterval == 0 {
		return DefaultLeaseExtendInterval
	}
	return o.LeaseExtendInterval
}

// RunWorker starts a blocking loop that polls the task queue and executes
// workflows. It returns when the context is cancelled or a fatal error occurs.
//
// Each iteration:
//  1. Poll the queue for a ready task.
//  2. Look up the workflow by name in the runner's registry.
//  3. Call the workflow's Run method (replay + execute).
//  4. Mark the task as complete or failed.
//
// When a workflow returns ErrWorkflowSleeping, the task is marked complete
// because Sleep already enqueued a delayed follow-up task.
//
//nolint:gocognit
func (r *Runner) RunWorker(ctx context.Context, opts WorkerOptions) error {
	interval := opts.pollInterval()
	leaseExtend := opts.leaseExtendInterval()
	r.logger.InfoContext(ctx, "worker starting", "pollInterval", interval, "leaseExtend", leaseExtend)

	for {
		task, err := r.queue.Poll(ctx)
		if err != nil {
			return fmt.Errorf("poll queue: %w", err)
		}

		if task == nil {
			// Nothing ready — wait before polling again.
			select {
			case <-time.After(interval):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		wf, ok := r.workflows[task.WorkflowName]
		if !ok {
			r.logger.ErrorContext(ctx,
				"unknown workflow, failing task",
				"instanceID", task.InstanceID,
				"workflowName", task.WorkflowName,
			)
			_ = r.queue.Fail(ctx, task.InstanceID)
			continue
		}

		r.logger.DebugContext(ctx,
			"worker executing task",
			"instanceID", task.InstanceID,
			"workflowName", task.WorkflowName,
		)

		// Spawn a background goroutine that periodically extends the lease
		// so long-running steps don't cause lease expiry and task re-claiming.
		leaseCtx, leaseCancel := context.WithCancel(ctx)
		go r.autoExtendLease(leaseCtx, task.InstanceID, leaseExtend)

		err = wf.execute(ctx, task.InstanceID)
		leaseCancel() // stop the lease extender
		//nolint:nestif // It's readable.
		if err != nil {
			if isConflictAbortError(err) {
				// Another worker already advanced this instance. Our execution
				// is stale — complete the task and move on.
				r.logger.InfoContext(ctx,
					"conflict abort — task completed by another worker",
					"instanceID", task.InstanceID,
				)
				r.completeTask(ctx, task.InstanceID)
				continue
			}
			if isSleepError(err) {
				// Sleep already enqueued a delayed task — this execution is done.
				r.completeTask(ctx, task.InstanceID)
				continue
			}
			if isRetryError(err) {
				// Retry already enqueued a delayed task — this execution is done.
				r.completeTask(ctx, task.InstanceID)
				continue
			}
			if isWaitingError(err) {
				// WaitForEvent parked the workflow — it will be woken by PublishEvent.
				r.completeTask(ctx, task.InstanceID)
				continue
			}
			if isCancelledError(err) {
				// Workflow was cancelled — clean up the task.
				r.completeTask(ctx, task.InstanceID)
				continue
			}
			r.logger.ErrorContext(ctx,
				"workflow execution failed",
				"instanceID", task.InstanceID,
				"error", err,
			)
			if qErr := r.queue.Fail(ctx, task.InstanceID); qErr != nil {
				r.logger.ErrorContext(
					ctx,
					"failed to mark task as failed",
					"instanceID",
					task.InstanceID,
					"error",
					qErr,
				)
			}
			continue
		}

		r.completeTask(ctx, task.InstanceID)
	}
}

// autoExtendLease periodically extends the task's lease until the context is cancelled.
// This runs as a background goroutine during workflow execution to prevent other workers
// from reclaiming the task during long-running steps.
func (r *Runner) autoExtendLease(ctx context.Context, instanceID InstanceID, interval time.Duration) {
	// Extend lease by 3x the interval to provide safety margin.
	leaseExtension := interval * 3 //nolint:mnd // Extension is 3x the check interval.

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.queue.ExtendLease(ctx, instanceID, leaseExtension); err != nil {
				// Context cancelled is expected when the execution finishes.
				if ctx.Err() != nil {
					return
				}
				r.logger.WarnContext(ctx,
					"failed to extend lease",
					"instanceID", instanceID,
					"error", err,
				)
			}
		}
	}
}

// completeTask marks a task as done, logging on failure.
func (r *Runner) completeTask(ctx context.Context, instanceID InstanceID) {
	if err := r.queue.Complete(ctx, instanceID); err != nil {
		r.logger.ErrorContext(
			ctx,
			"failed to complete task",
			"instanceID",
			instanceID,
			"error",
			err,
		)
	}
}
