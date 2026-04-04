package workflow

import (
	"context"
	"fmt"
	"time"
)

const (
	DefaultPollInterval = 200 * time.Millisecond
)

// WorkerOptions configures a worker started via RunWorker.
type WorkerOptions struct {
	// PollInterval is how long the worker waits between queue polls
	// when no tasks are ready. Defaults to 200ms.
	PollInterval time.Duration
}

func (o WorkerOptions) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return o.PollInterval
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
func (r *Runner) RunWorker(ctx context.Context, opts WorkerOptions) error {
	interval := opts.pollInterval()
	r.logger.Info("worker starting", "pollInterval", interval)

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
			r.logger.Error(
				"unknown workflow, failing task",
				"instanceID", task.InstanceID,
				"workflowName", task.WorkflowName,
			)
			_ = r.queue.Fail(ctx, task.InstanceID)
			continue
		}

		r.logger.Debug(
			"worker executing task",
			"instanceID", task.InstanceID,
			"workflowName", task.WorkflowName,
		)

		err = wf.execute(ctx, task.InstanceID)
		if err != nil {
			if isSleepError(err) {
				// Sleep already enqueued a delayed task — this execution is done.
				_ = r.queue.Complete(ctx, task.InstanceID)
				continue
			}
			r.logger.Error(
				"workflow execution failed",
				"instanceID", task.InstanceID,
				"error", err,
			)
			_ = r.queue.Fail(ctx, task.InstanceID)
			continue
		}

		_ = r.queue.Complete(ctx, task.InstanceID)
	}
}
