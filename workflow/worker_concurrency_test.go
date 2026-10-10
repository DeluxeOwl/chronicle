package workflow_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/eventlog"
	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestRunWorkers_BoundsActiveExecutions(t *testing.T) {
	queue := workflow.NewMemoryQueue()
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)

	const concurrency = 3
	const instances = 8
	started := make(chan struct{}, instances)
	release := make(chan struct{})
	wf := workflow.New(
		runner,
		"bounded-workers",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				started <- struct{}{}
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			return &struct{}{}, err
		},
	)

	ids := make([]workflow.InstanceID, instances)
	for i := range ids {
		ids[i], err = wf.Start(t.Context(), &struct{}{})
		require.NoError(t, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorkers(ctx, concurrency, workflow.WorkerOptions{
			PollInterval:        time.Millisecond,
			LeaseExtendInterval: -1,
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("worker pool did not shut down")
		}
	})

	for range concurrency {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not execute concurrently")
		}
	}
	require.Equal(
		t,
		instances-concurrency,
		queue.Len(),
		"workers must not prefetch beyond available execution slots",
	)
	select {
	case <-started:
		t.Fatal("worker pool exceeded its concurrency limit")
	default:
	}

	close(release)
	require.Eventually(t, func() bool {
		for _, id := range ids {
			if _, err := wf.GetResult(t.Context(), id); err != nil {
				return false
			}
		}
		return true
	}, 2*time.Second, time.Millisecond)
}

func TestRunWorkers_ShutdownLeavesMemoryTaskRunnable(t *testing.T) {
	queue := workflow.NewMemoryQueue()
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	started := make(chan struct{})
	var attempts atomic.Int32
	wf := workflow.New(
		runner,
		"shutdown-recovery",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				if attempts.Add(1) == 1 {
					close(started)
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorkers(ctx, 2, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("workflow was not picked up")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("worker shutdown did not join execution")
	}
	require.Equal(t, 1, queue.Len(), "shutdown must not lose the claimed memory task")
	info, err := wf.GetStatus(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, workflow.StatusRunning, info.Status, "shutdown is not a workflow failure")

	recoveryCtx, stop := context.WithCancel(t.Context())
	recoveryDone := make(chan error, 1)
	go func() {
		recoveryDone <- runner.RunWorker(recoveryCtx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	t.Cleanup(func() { stop(); <-recoveryDone })
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(t.Context(), id)
		return err == nil
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(2), attempts.Load(), "the interrupted step must be resumed")
}

func TestRunWorker_UnknownWorkflowRetainedUntilRegistered(t *testing.T) {
	queue := workflow.NewMemoryQueue()
	log := eventlog.NewMemory()
	creator, err := workflow.NewRunner(log, nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	fn := func(_ *workflow.Context, _ *struct{}) (*struct{}, error) { return &struct{}{}, nil }
	original := workflow.New(creator, "late-registration", fn)
	id, err := original.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	runner, err := workflow.NewRunner(log, nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	err = runner.RunWorker(
		ctx,
		workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1},
	)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, queue.Len(), "an unknown registration must not discard the task")
	registered := workflow.New(runner, "late-registration", fn)
	ctx2, cancel2 := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx2, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	t.Cleanup(func() { cancel2(); <-done })
	require.Eventually(t, func() bool {
		_, err := registered.GetResult(t.Context(), id)
		return err == nil
	}, time.Second, time.Millisecond)
}

func TestRunWorker_InfrastructureFailureRetainsTask(t *testing.T) {
	queue := workflow.NewMemoryQueue()
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	var attempts atomic.Int32
	wf := workflow.New(
		runner,
		"infrastructure-recovery",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(_ context.Context) error {
				if attempts.Add(1) == 1 {
					return errors.Join(
						workflow.ErrWorkflowInfrastructure,
						errors.New("storage unavailable"),
					)
				}
				return nil
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(t.Context(), id)
		return err == nil
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(2), attempts.Load())
}

type transientPollQueue struct {
	workflow.TaskQueue
	failed atomic.Bool
}

func (q *transientPollQueue) Poll(ctx context.Context) (*workflow.QueuedTask, error) {
	if q.failed.CompareAndSwap(false, true) {
		return nil, errors.New("temporary queue failure")
	}
	return q.TaskQueue.Poll(ctx)
}

func TestRunWorker_RetriesTransientLegacyQueueError(t *testing.T) {
	queue := &transientPollQueue{TaskQueue: workflow.NewMemoryQueue(), failed: atomic.Bool{}}
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"legacy-queue",
		func(_ *workflow.Context, _ *struct{}) (*struct{}, error) { return &struct{}{}, nil },
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: -1})
	}()
	t.Cleanup(func() { cancel(); <-done })
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(t.Context(), id)
		return err == nil
	}, time.Second, time.Millisecond)
}

type blockingHeartbeatQueue struct {
	workflow.ClaimedTaskQueue
	started chan struct{}
	exited  chan struct{}
	retired chan bool
}

func (q *blockingHeartbeatQueue) ExtendClaim(
	ctx context.Context,
	_ workflow.ClaimedTask,
	_ time.Duration,
) error {
	close(q.started)
	<-ctx.Done()
	close(q.exited)
	return ctx.Err()
}

func (q *blockingHeartbeatQueue) CompleteClaim(
	ctx context.Context,
	task workflow.ClaimedTask,
) error {
	select {
	case <-q.exited:
		q.retired <- true
	default:
		q.retired <- false
	}
	return q.ClaimedTaskQueue.CompleteClaim(ctx, task)
}

func TestRunWorker_JoinsHeartbeatBeforeAcknowledgment(t *testing.T) {
	queue := &blockingHeartbeatQueue{
		ClaimedTaskQueue: workflow.NewMemoryQueue(),
		started: make(
			chan struct{},
		), exited: make(chan struct{}), retired: make(chan bool, 1),
	}
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	wf := workflow.New(
		runner,
		"join-heartbeat",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			select {
			case <-queue.started:
				return &struct{}{}, nil
			case <-wctx.Done():
				return nil, wctx.Err()
			}
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: time.Millisecond})
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case joined := <-queue.retired:
		require.True(t, joined, "the active heartbeat must exit before retiring its receipt")
	case <-time.After(time.Second):
		t.Fatal("heartbeat prevented completion or was not joined")
	}
	_, err = wf.GetResult(t.Context(), id)
	require.NoError(t, err)
}

type losingHeartbeatQueue struct {
	*workflow.MemoryQueue
	revoked atomic.Bool
}

func (q *losingHeartbeatQueue) ExtendClaim(
	ctx context.Context,
	task workflow.ClaimedTask,
	lease time.Duration,
) error {
	if q.revoked.CompareAndSwap(false, true) {
		if err := q.ReleaseClaim(ctx, task, time.Time{}); err != nil {
			return err
		}
		return workflow.ErrStaleClaim
	}
	return q.MemoryQueue.ExtendClaim(ctx, task, lease)
}

func TestRunWorker_LostHeartbeatInterruptsExecutionAndPreservesTask(t *testing.T) {
	queue := &losingHeartbeatQueue{MemoryQueue: workflow.NewMemoryQueue(), revoked: atomic.Bool{}}
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	interrupted := make(chan struct{})
	var attempts atomic.Int32
	wf := workflow.New(
		runner,
		"lost-heartbeat",
		func(wctx *workflow.Context, _ *struct{}) (*struct{}, error) {
			err := workflow.Step2(wctx, func(ctx context.Context) error {
				if attempts.Add(1) == 1 {
					<-ctx.Done()
					close(interrupted)
					return ctx.Err()
				}
				return nil
			})
			return &struct{}{}, err
		},
	)
	id, err := wf.Start(t.Context(), &struct{}{})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorker(ctx, workflow.WorkerOptions{PollInterval: time.Millisecond, LeaseExtendInterval: time.Millisecond})
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("lost claim did not interrupt the workflow context")
	}
	require.Eventually(t, func() bool {
		_, err := wf.GetResult(t.Context(), id)
		return err == nil
	}, time.Second, time.Millisecond)
	require.Equal(
		t,
		int32(2),
		attempts.Load(),
		"lost ownership must remain recoverable without a failure event",
	)
}

type observedIdleQueue struct {
	workflow.ClaimedTaskQueue
	polled chan struct{}
}

func (q *observedIdleQueue) PollClaim(ctx context.Context) (*workflow.ClaimedTask, error) {
	task, err := q.ClaimedTaskQueue.PollClaim(ctx)
	if err == nil && task == nil {
		q.polled <- struct{}{}
	}
	return task, err
}

func TestRunWorkers_IdleWaitIsContextAware(t *testing.T) {
	queue := &observedIdleQueue{
		ClaimedTaskQueue: workflow.NewMemoryQueue(),
		polled:           make(chan struct{}, 4),
	}
	runner, err := workflow.NewRunner(eventlog.NewMemory(), nil, workflow.WithTaskQueue(queue))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runner.RunWorkers(ctx, 4, workflow.WorkerOptions{PollInterval: time.Hour, LeaseExtendInterval: -1})
	}()
	defer cancel()
	for range 4 {
		select {
		case <-queue.polled:
		case <-time.After(time.Second):
			t.Fatal("workers did not reach their empty-poll wait")
		}
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("worker pool did not interrupt its idle waits")
	}
	require.Error(
		t,
		runner.RunWorkers(
			t.Context(),
			0,
			workflow.WorkerOptions{PollInterval: 0, LeaseExtendInterval: 0},
		),
	)
}
