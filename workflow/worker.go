package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	DefaultPollInterval        = 200 * time.Millisecond
	DefaultLeaseExtendInterval = 10 * time.Second
	workerCleanupTimeout       = 5 * time.Second
)

// WorkerOptions configures RunWorker and RunWorkers.
type WorkerOptions struct {
	// PollInterval is the delay after an empty poll or infrastructure error.
	// Defaults to 200ms. All waits are interrupted by context cancellation.
	PollInterval time.Duration

	// LeaseExtendInterval controls automatic heartbeats. Defaults to 10s.
	// Built-in queues cap this at one third of their initial lease duration.
	// A negative value disables automatic extension; long-running steps must
	// then call Heartbeat themselves.
	LeaseExtendInterval time.Duration
}

func (o WorkerOptions) pollInterval() time.Duration {
	if o.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return o.PollInterval
}

func (o WorkerOptions) leaseExtendInterval() time.Duration {
	if o.LeaseExtendInterval < 0 {
		return 0
	}
	if o.LeaseExtendInterval == 0 {
		return DefaultLeaseExtendInterval
	}
	return o.LeaseExtendInterval
}

// RunWorker is the single-worker form of RunWorkers.
func (r *Runner) RunWorker(ctx context.Context, opts WorkerOptions) error {
	return r.RunWorkers(ctx, 1, opts)
}

// RunWorkers runs at most concurrency workflow executions at once. It blocks
// until cancellation, joining all executions and their automatic heartbeats.
// Workflow functions and custom queue implementations must honor their context
// for shutdown to finish. Transient queue errors are logged and retried.
//
// Built-in queues use distinct claim receipts. Legacy TaskQueue implementations
// remain supported, but their ID-only operations cannot fence overlapping claims.
// Workers retry legacy requeues for up to five seconds, including during shutdown.
// If requeueing still fails, RunWorkers returns an error with the instance ID.
// A destructive legacy queue cannot recover that task after a process exit.
// Queue ownership does not make external workflow effects exactly once.
func (r *Runner) RunWorkers(ctx context.Context, concurrency int, opts WorkerOptions) error {
	if concurrency <= 0 {
		return fmt.Errorf("worker concurrency must be positive: %d", concurrency)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.logger.InfoContext(ctx, "workers starting", "concurrency", concurrency,
		"pollInterval", opts.pollInterval(), "leaseExtend", opts.leaseExtendInterval())
	group, workerCtx := errgroup.WithContext(ctx)
	retentionErrors := make(chan error, concurrency)
	for range concurrency {
		group.Go(func() error {
			err := r.runWorker(workerCtx, opts)
			if errors.Is(err, errLegacyTaskRetention) {
				retentionErrors <- err
			}
			return err
		})
	}
	err := group.Wait()
	close(retentionErrors)
	// An idle worker can return cancellation before another worker's cleanup
	// fails. The first errgroup error must not hide an unretained task.
	for retentionErr := range retentionErrors {
		if !errors.Is(err, retentionErr) {
			err = errors.Join(err, retentionErr)
		}
	}
	return err
}

var errLegacyTaskRetention = errors.New("legacy queue could not retain unfinished task")

type workerTask struct {
	QueuedTask
	claim *ClaimedTask
}

func (r *Runner) pollWorkerTask(ctx context.Context) (*workerTask, error) {
	if queue, ok := r.queue.(ClaimedTaskQueue); ok {
		claim, err := queue.PollClaim(ctx)
		if err != nil || claim == nil {
			return nil, err
		}
		return &workerTask{QueuedTask: claim.QueuedTask, claim: claim}, nil
	}
	task, err := r.queue.Poll(ctx)
	if err != nil || task == nil {
		return nil, err
	}
	return &workerTask{QueuedTask: *task, claim: nil}, nil
}

func (r *Runner) runWorker(ctx context.Context, opts WorkerOptions) error {
	for ctx.Err() == nil {
		task, err := r.pollWorkerTask(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.logger.WarnContext(ctx, "queue poll failed; retrying", "error", err)
			}
			if err := waitForWorker(ctx, opts.pollInterval()); err != nil {
				return err
			}
			continue
		}
		if task == nil {
			if err := waitForWorker(ctx, opts.pollInterval()); err != nil {
				return err
			}
			continue
		}
		if err := r.executeWorkerTask(ctx, task, opts); err != nil {
			if errors.Is(err, errLegacyTaskRetention) {
				return err
			}
			if ctx.Err() == nil {
				r.logger.WarnContext(ctx, "task unfinished; retained for retry",
					"instanceID", task.InstanceID, "workflowName", task.WorkflowName, "error", err)
			}
			if err := waitForWorker(ctx, opts.pollInterval()); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func waitForWorker(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Runner) executeWorkerTask(
	ctx context.Context,
	task *workerTask,
	opts WorkerOptions,
) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, r.releaseWorkerTask(ctx, task, opts.pollInterval()))
	}
	wf, ok := r.lookupWorkflow(task.WorkflowName)
	if !ok {
		err := fmt.Errorf("unknown workflow %q", task.WorkflowName)
		return errors.Join(err, r.releaseWorkerTask(ctx, task, opts.pollInterval()))
	}

	executionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	lease := &workerLease{queue: r.queue, task: task, cancel: cancel}
	executionCtx = context.WithValue(executionCtx, workerLeaseContextKey{}, lease)
	stopHeartbeat := r.startWorkerHeartbeat(executionCtx, lease, opts.leaseExtendInterval())
	r.logger.DebugContext(executionCtx, "worker executing task",
		"instanceID", task.InstanceID, "workflowName", task.WorkflowName)
	err := func() error {
		defer stopHeartbeat() // Join before acknowledgment or release, including on panic.
		return wf.execute(executionCtx, task.InstanceID)
	}()

	if executionCtx.Err() != nil {
		return errors.Join(
			context.Cause(executionCtx),
			r.releaseWorkerTask(ctx, task, opts.pollInterval()),
		)
	}
	if isSleepError(err) || isRetryError(err) {
		return r.deferWorkerTask(ctx, task, opts.pollInterval())
	}
	if err == nil || isWaitingError(err) || isCancelledError(err) {
		return r.finishWorkerTask(ctx, task, false, opts.pollInterval())
	}
	if errors.Is(err, ErrWorkflowInfrastructure) {
		return errors.Join(err, r.releaseWorkerTask(ctx, task, opts.pollInterval()))
	}

	// An execution error (including a concurrency conflict) is not evidence
	// of a durable terminal state. Never discard an intermediate checkpoint.
	instance, loadErr := r.repo.Get(executionCtx, task.InstanceID)
	if loadErr != nil {
		return errors.Join(err, loadErr, r.releaseWorkerTask(ctx, task, opts.pollInterval()))
	}
	if isTerminalWorkerStatus(instance.status) {
		return r.finishWorkerTask(ctx, task, instance.status == StatusFailed, opts.pollInterval())
	}
	return errors.Join(err, r.releaseWorkerTask(ctx, task, opts.pollInterval()))
}

// A readiness guard can return a delay sentinel without scheduling a task
// in this execution. Release at the durable deadline instead of deleting it.
func (r *Runner) deferWorkerTask(
	ctx context.Context,
	task *workerTask,
	retryDelay time.Duration,
) error {
	instance, err := r.repo.Get(ctx, task.InstanceID)
	if err != nil {
		return errors.Join(err, r.releaseWorkerTask(ctx, task, retryDelay))
	}
	if isTerminalWorkerStatus(instance.status) {
		return r.finishWorkerTask(ctx, task, instance.status == StatusFailed, retryDelay)
	}
	if instance.nextRunAfter.IsZero() {
		return r.releaseWorkerTask(ctx, task, retryDelay)
	}
	return r.releaseWorkerTaskAt(ctx, task, instance.nextRunAfter)
}

func isTerminalWorkerStatus(status Status) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusCancelled
}

func (r *Runner) finishWorkerTask(
	ctx context.Context,
	task *workerTask,
	failed bool,
	retryDelay time.Duration,
) error {
	var err error
	if queue, ok := r.queue.(ClaimedTaskQueue); ok && task.claim != nil {
		if failed {
			err = queue.FailClaim(ctx, *task.claim)
		} else {
			err = queue.CompleteClaim(ctx, *task.claim)
		}
	} else if failed {
		err = r.queue.Fail(ctx, task.InstanceID)
	} else {
		err = r.queue.Complete(ctx, task.InstanceID)
	}
	if errors.Is(err, ErrStaleClaim) {
		// A transactional terminal event may already have removed the row.
		// An expired or replaced receipt must never acknowledge a newer claim.
		r.logger.DebugContext(
			ctx,
			"task claim already retired or expired",
			"instanceID",
			task.InstanceID,
		)
		return nil
	}
	if err != nil {
		return errors.Join(
			fmt.Errorf("acknowledge task: %w", err),
			r.releaseWorkerTask(ctx, task, retryDelay),
		)
	}
	return nil
}

func (r *Runner) releaseWorkerTask(
	ctx context.Context,
	task *workerTask,
	retryDelay time.Duration,
) error {
	return r.releaseWorkerTaskAt(ctx, task, laterDeadline(task.RunAfter, r.now().Add(retryDelay)))
}

func (r *Runner) releaseWorkerTaskAt(
	ctx context.Context,
	task *workerTask,
	runAfter time.Time,
) error {
	// Cancellation must not make destructive memory polls lose unfinished work.
	// Detached cleanup is bounded, and only ever releases this exact receipt.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerCleanupTimeout)
	defer cancel()
	var err error
	if task.claim != nil {
		if queue, ok := r.queue.(ClaimedTaskReleaser); ok {
			err = queue.ReleaseClaim(cleanupCtx, *task.claim, runAfter)
		}
		// Otherwise leave the recoverable claim to expire.
	} else {
		retry := task.QueuedTask
		retry.RunAfter = runAfter
		return r.requeueLegacyWorkerTask(cleanupCtx, retry)
	}
	if errors.Is(err, ErrStaleClaim) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release unfinished task: %w", err)
	}
	return nil
}

func (r *Runner) requeueLegacyWorkerTask(ctx context.Context, task QueuedTask) error {
	for {
		err := r.queue.Enqueue(ctx, task)
		if err == nil {
			return nil
		}
		if waitErr := waitForWorker(ctx, DefaultPollInterval); waitErr != nil {
			return fmt.Errorf(
				"%w %s: %w",
				errLegacyTaskRetention,
				task.InstanceID,
				errors.Join(err, waitErr),
			)
		}
	}
}

type workerLeaseContextKey struct{}

type workerLease struct {
	queue  TaskQueue
	task   *workerTask
	cancel context.CancelCauseFunc
}

func (l *workerLease) extend(ctx context.Context, duration time.Duration) error {
	var err error
	if queue, ok := l.queue.(ClaimedTaskQueue); ok && l.task.claim != nil {
		err = queue.ExtendClaim(ctx, *l.task.claim, duration)
	} else {
		err = l.queue.ExtendLease(ctx, l.task.InstanceID, duration)
	}
	if err != nil && ctx.Err() == nil {
		// Losing ownership or being unable to renew it interrupts execution.
		// The execution layer must not turn context shutdown into domain failure.
		l.cancel(err)
	}
	return err
}

func (r *Runner) startWorkerHeartbeat(
	ctx context.Context,
	lease *workerLease,
	interval time.Duration,
) func() {
	if interval <= 0 {
		return func() {}
	}
	if queue, ok := r.queue.(interface{ claimLeaseDuration() time.Duration }); ok {
		interval = min(interval, queue.claimLeaseDuration()/3)
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.autoExtendWorkerLease(heartbeatCtx, lease, interval)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (r *Runner) autoExtendWorkerLease(
	ctx context.Context,
	lease *workerLease,
	interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := lease.extend(ctx, interval*3); err != nil {
				if ctx.Err() == nil {
					r.logger.WarnContext(ctx, "task heartbeat failed; interrupting execution",
						"instanceID", lease.task.InstanceID, "error", err)
				}
				return
			}
		}
	}
}
