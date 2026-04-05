package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrWorkflowSleeping is returned by Run when the workflow is currently sleeping.
// The caller should not treat this as a failure — the workflow will be automatically
// re-triggered by the task queue when the sleep duration elapses.
var ErrWorkflowSleeping = errors.New("workflow is sleeping")

// sleepResult is the data stored in the step result for a sleep step.
// It records the absolute wake-up time so that on replay, the workflow
// knows whether the sleep has elapsed regardless of when it's re-executed.
type sleepResult struct {
	WakeAt   time.Time     `json:"wakeAt"`
	Duration time.Duration `json:"duration"`
}

// Sleep pauses the workflow for the given duration.
//
// The sleep is durable: the wake-up time is recorded as an event in the event log.
// If the process restarts, calling Run again on this instance will check the
// recorded wake time against the current clock and either continue sleeping
// or resume execution.
//
// When a workflow hits a sleep that hasn't elapsed yet, Run returns
// ErrWorkflowSleeping. A delayed task is enqueued on the Runner's TaskQueue
// so that a worker will automatically re-run the workflow once the sleep
// duration has passed.
func Sleep(wctx *Context, d time.Duration) error {
	runner := wctx.runner

	// Claim the step index from the local counter
	stepIndex := wctx.stepCount
	wctx.stepCount++

	// Reload instance to get latest state
	instance, err := runner.repo.Get(wctx.ctx, wctx.instanceID)
	if err != nil {
		return fmt.Errorf("reload instance for sleep: %w", err)
	}

	now := runner.now()

	// Check if this sleep step was already recorded (replay)
	if cachedResult, ok := instance.stepResults[stepIndex]; ok {
		var sr sleepResult
		if err := json.Unmarshal(cachedResult, &sr); err != nil {
			return fmt.Errorf("unmarshal cached sleep result: %w", err)
		}

		if now.Before(sr.WakeAt) {
			// Still sleeping — enqueue a delayed wake-up task
			if err := runner.queue.Enqueue(wctx.ctx, QueuedTask{
				InstanceID:   wctx.instanceID,
				WorkflowName: instance.workflowName,
				RunAfter:     sr.WakeAt,
			}); err != nil {
				return fmt.Errorf("enqueue sleep wake task: %w", err)
			}

			runner.logger.Debug(
				"sleep replay, still sleeping",
				"instanceID", wctx.instanceID,
				"stepIndex", stepIndex,
				"remaining", sr.WakeAt.Sub(now),
			)
			return ErrWorkflowSleeping
		}

		// Sleep has elapsed, continue execution
		runner.logger.Debug(
			"sleep replay, elapsed",
			"instanceID", wctx.instanceID,
			"stepIndex", stepIndex,
		)
		return nil
	}

	// First execution: record the sleep
	wakeAt := now.Add(d)
	sr := sleepResult{
		WakeAt:   wakeAt,
		Duration: d,
	}
	resultJSON, err := json.Marshal(sr)
	if err != nil {
		return fmt.Errorf("marshal sleep result: %w", err)
	}

	if err := instance.recordThat(&stepCompleted{
		StepIndex:   stepIndex,
		Result:      resultJSON,
		CompletedAt: now,
	}); err != nil {
		return fmt.Errorf("record sleep step: %w", err)
	}

	if _, _, err := runner.repo.Save(wctx.ctx, instance); err != nil {
		return fmt.Errorf("save sleep step: %w", err)
	}

	// Enqueue a delayed task for the wake-up
	if err := runner.queue.Enqueue(wctx.ctx, QueuedTask{
		InstanceID:   wctx.instanceID,
		WorkflowName: instance.workflowName,
		RunAfter:     wakeAt,
	}); err != nil {
		return fmt.Errorf("enqueue sleep wake task: %w", err)
	}

	runner.logger.Info(
		"workflow sleeping",
		"instanceID", wctx.instanceID,
		"stepIndex", stepIndex,
		"duration", d,
		"wakeAt", wakeAt,
	)

	return ErrWorkflowSleeping
}

// isSleepError checks if an error is ErrWorkflowSleeping.
func isSleepError(err error) bool {
	return errors.Is(err, ErrWorkflowSleeping)
}

// RunnerOption configures a Runner.
type RunnerOption func(*Runner)

// WithNowFunc sets a custom clock function for the runner.
// Defaults to time.Now. Useful for testing time-dependent behavior
// like sleep without waiting for real time to pass.
func WithNowFunc(fn func() time.Time) RunnerOption {
	return func(r *Runner) {
		r.nowFunc = fn
	}
}

// WithTaskQueue sets a custom task queue for the runner.
// Defaults to an in-memory queue.
func WithTaskQueue(q TaskQueue) RunnerOption {
	return func(r *Runner) {
		r.queue = q
	}
}

// now returns the current time using the configured clock.
func (r *Runner) now() time.Time {
	return r.nowFunc()
}
