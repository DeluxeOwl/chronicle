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
func Sleep(wctx *Context, d time.Duration, opts ...StepOption) error {
	config, err := configureStep(opts)
	if err != nil {
		return infrastructureError(err)
	}
	if d < 0 {
		return infrastructureError(errors.New("sleep duration must be nonnegative"))
	}
	index := wctx.stepCount
	wctx.stepCount++
	if err := validateStep(wctx, index, stepKindSleep, config); err != nil {
		return err
	}
	var sleep sleepResult
	//nolint:nestif // The first sleep saves a deadline while replay reads it.
	if cached, ok := wctx.instance.stepResults[index]; ok {
		if err := json.Unmarshal(cached, &sleep); err != nil {
			return infrastructureError(fmt.Errorf("decode sleep checkpoint: %w", err))
		}
		if sleep.WakeAt.IsZero() {
			return infrastructureError(errors.New("sleep checkpoint has no deadline"))
		}
	} else {
		now := wctx.runner.now()
		sleep = sleepResult{WakeAt: now.Add(d), Duration: d}
		encoded, err := json.Marshal(sleep)
		if err != nil {
			return infrastructureError(err)
		}
		if err := wctx.instance.recordThat(&stepCompleted{StepIndex: index, Result: encoded, CompletedAt: now, WorkflowName: wctx.instance.workflowName, Name: config.name, Kind: stepKindSleep}); err != nil {
			return infrastructureError(err)
		}
		if err := saveOrConflictAbort(wctx.ctx, wctx.runner.repo, wctx.instance); err != nil {
			return err
		}
	}
	if !wctx.runner.now().Before(sleep.WakeAt) {
		return nil
	}
	if err := wctx.runner.enqueueTask(wctx.ctx, QueuedTask{InstanceID: wctx.instanceID, WorkflowName: wctx.instance.workflowName, RunAfter: sleep.WakeAt}); err != nil {
		return err
	}
	wctx.runner.logger.DebugContext(
		wctx.ctx,
		"workflow sleeping",
		"instanceID",
		wctx.instanceID,
		"step",
		index,
		"wakeAt",
		sleep.WakeAt,
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

// WithSyncQueueOpts passes SyncQueue options through to
// NewSqliteRunnerWithSyncQueue. Ignored by NewRunner.
func WithSyncQueueOpts(opts ...SyncQueueOption) RunnerOption {
	return func(r *Runner) {
		r.syncQueueOpts = append(r.syncQueueOpts, opts...)
	}
}

// now returns the current time using the configured clock.
func (r *Runner) now() time.Time {
	return r.nowFunc().UTC()
}
