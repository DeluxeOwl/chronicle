package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrWorkflowSleeping is returned by Run when the workflow is currently sleeping.
// The caller should not treat this as a failure — the workflow will be automatically
// re-triggered by the scheduler when the sleep duration elapses.
var ErrWorkflowSleeping = errors.New("workflow is sleeping")

// sleepResult is the data stored in the step result for a sleep step.
// It records the absolute wake-up time so that on replay, the workflow
// knows whether the sleep has elapsed regardless of when it's re-executed.
type sleepResult struct {
	WakeAt   time.Time     `json:"wakeAt"`
	Duration time.Duration `json:"duration"`
}

// Scheduler controls how sleeping workflows get re-triggered after their
// duration elapses. The default implementation uses time.AfterFunc.
// Users can implement this interface to use persistent job queues,
// distributed schedulers, cron systems, etc.
type Scheduler interface {
	// Schedule arranges for fn to be called after duration d elapses.
	// The returned func cancels the scheduled call if it hasn't fired yet.
	Schedule(d time.Duration, fn func()) func()
}

// timerScheduler is the default Scheduler backed by time.AfterFunc.
type timerScheduler struct{}

func (timerScheduler) Schedule(d time.Duration, fn func()) func() {
	t := time.AfterFunc(d, fn)
	return func() { t.Stop() }
}

// sleepWakeEntry tracks a pending sleep wake-up so it can be cancelled on Close.
type sleepWakeEntry struct {
	cancel func()
}

// Sleep pauses the workflow for the given duration.
//
// The sleep is durable: the wake-up time is recorded as an event in the event log.
// If the process restarts, calling Run again on this instance will check the
// recorded wake time against the current clock and either continue sleeping
// or resume execution.
//
// When a workflow hits a sleep that hasn't elapsed yet, Run returns
// ErrWorkflowSleeping. The Runner's scheduler automatically re-triggers
// Run when the duration elapses.
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
			// Still sleeping — schedule a wake-up for the remaining time
			remaining := sr.WakeAt.Sub(now)
			runner.scheduleSleepWake(wctx.instanceID, remaining)

			runner.logger.Debug(
				"sleep replay, still sleeping",
				"instanceID", wctx.instanceID,
				"stepIndex", stepIndex,
				"remaining", remaining,
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
		StepIndex: stepIndex,
		Result:    resultJSON,
	}); err != nil {
		return fmt.Errorf("record sleep step: %w", err)
	}

	if _, _, err := runner.repo.Save(wctx.ctx, instance); err != nil {
		return fmt.Errorf("save sleep step: %w", err)
	}

	// Schedule the wake-up
	runner.scheduleSleepWake(wctx.instanceID, d)

	runner.logger.Info(
		"workflow sleeping",
		"instanceID", wctx.instanceID,
		"stepIndex", stepIndex,
		"duration", d,
		"wakeAt", wakeAt,
	)

	return ErrWorkflowSleeping
}

// scheduleSleepWake schedules a re-run of the workflow after the given duration.
// It uses the Runner's base context (not the caller's context) since the
// original request context may be long gone by the time the sleep elapses.
func (r *Runner) scheduleSleepWake(instanceID InstanceID, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Cancel any existing wake for this instance
	if entry, ok := r.sleepWakes[instanceID]; ok {
		entry.cancel()
		delete(r.sleepWakes, instanceID)
	}

	cancel := r.scheduler.Schedule(d, func() {
		r.wg.Add(1)
		defer r.wg.Done()

		r.mu.Lock()
		delete(r.sleepWakes, instanceID)
		r.mu.Unlock()

		// Use the runner's base context
		ctx := r.baseCtx
		if ctx.Err() != nil {
			return // Runner has been closed
		}

		r.logger.Debug("sleep wake triggered", "instanceID", instanceID)

		// Notify via the wake channel if anyone is listening
		r.mu.Lock()
		ch, ok := r.wakeChs[instanceID]
		r.mu.Unlock()
		if ok {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	})

	r.sleepWakes[instanceID] = sleepWakeEntry{cancel: cancel}
}

// WaitForWake blocks until the given workflow instance is woken from sleep,
// or the context is cancelled. This is primarily useful for testing.
func (r *Runner) WaitForWake(ctx context.Context, instanceID InstanceID) error {
	r.mu.Lock()
	ch, ok := r.wakeChs[instanceID]
	if !ok {
		ch = make(chan struct{}, 1)
		r.wakeChs[instanceID] = ch
	}
	r.mu.Unlock()

	select {
	case <-ch:
		r.mu.Lock()
		delete(r.wakeChs, instanceID)
		r.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close cancels all pending sleep wake-ups and waits for in-flight
// callbacks to finish. After Close returns, no more scheduled wake-ups
// will fire.
func (r *Runner) Close() {
	r.closeOnce.Do(func() {
		r.cancelBase()

		r.mu.Lock()
		for id, entry := range r.sleepWakes {
			entry.cancel()
			delete(r.sleepWakes, id)
		}
		// Close all wake channels
		for id, ch := range r.wakeChs {
			close(ch)
			delete(r.wakeChs, id)
		}
		r.mu.Unlock()

		r.wg.Wait()
	})
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

// WithScheduler sets a custom scheduler for the runner.
// Defaults to a time.AfterFunc-based scheduler.
func WithScheduler(s Scheduler) RunnerOption {
	return func(r *Runner) {
		r.scheduler = s
	}
}

// now returns the current time using the configured clock.
func (r *Runner) now() time.Time {
	return r.nowFunc()
}

// runnerFields holds the additional fields needed for sleep support.
// These are embedded into the Runner struct via the modifications below.
type runnerFields struct {
	nowFunc    func() time.Time
	scheduler  Scheduler
	baseCtx    context.Context
	cancelBase context.CancelFunc
	mu         sync.Mutex
	sleepWakes map[InstanceID]sleepWakeEntry
	wakeChs    map[InstanceID]chan struct{}
	wg         sync.WaitGroup
	closeOnce  sync.Once
}
