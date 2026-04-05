package workflow_test

import (
	"sync"
	"testing"
	"time"

	"github.com/DeluxeOwl/chronicle/workflow"
	"github.com/stretchr/testify/require"
)

func TestMemoryQueue_EnqueueAndPoll(t *testing.T) {
	q := workflow.NewMemoryQueue()
	ctx := t.Context()

	err := q.Enqueue(ctx, workflow.QueuedTask{
		InstanceID:   "wf-1",
		WorkflowName: "test-wf",
	})
	require.NoError(t, err)
	require.Equal(t, 1, q.Len())

	task, err := q.Poll(ctx)
	require.NoError(t, err)
	require.NotNil(t, task)
	require.Equal(t, workflow.InstanceID("wf-1"), task.InstanceID)
	require.Equal(t, "test-wf", task.WorkflowName)
	require.Equal(t, 0, q.Len())
}

func TestMemoryQueue_PollEmptyReturnsNil(t *testing.T) {
	q := workflow.NewMemoryQueue()

	task, err := q.Poll(t.Context())
	require.NoError(t, err)
	require.Nil(t, task)
}

func TestMemoryQueue_RunAfter_NotReadyUntilTime(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	q := workflow.NewMemoryQueue(workflow.WithMemoryQueueNowFunc(clock.Now))
	ctx := t.Context()

	wakeAt := clock.Now().Add(1 * time.Hour)
	err := q.Enqueue(ctx, workflow.QueuedTask{
		InstanceID:   "wf-delayed",
		WorkflowName: "test",
		RunAfter:     wakeAt,
	})
	require.NoError(t, err)

	// Not ready yet
	task, err := q.Poll(ctx)
	require.NoError(t, err)
	require.Nil(t, task, "task should not be ready before RunAfter")

	// Advance past RunAfter
	clock.Advance(2 * time.Hour)

	task, err = q.Poll(ctx)
	require.NoError(t, err)
	require.NotNil(t, task)
	require.Equal(t, workflow.InstanceID("wf-delayed"), task.InstanceID)
}

func TestMemoryQueue_FIFO_Ordering(t *testing.T) {
	q := workflow.NewMemoryQueue()
	ctx := t.Context()

	for i := range 5 {
		err := q.Enqueue(ctx, workflow.QueuedTask{
			InstanceID:   workflow.InstanceID("wf-" + string(rune('a'+i))),
			WorkflowName: "test",
		})
		require.NoError(t, err)
	}

	require.Equal(t, 5, q.Len())

	for i := range 5 {
		task, err := q.Poll(ctx)
		require.NoError(t, err)
		require.NotNil(t, task)
		require.Equal(t, workflow.InstanceID("wf-"+string(rune('a'+i))), task.InstanceID)
	}

	require.Equal(t, 0, q.Len())
}

func TestMemoryQueue_ImmediateTaskBeforeDelayed(t *testing.T) {
	clock := newClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	q := workflow.NewMemoryQueue(workflow.WithMemoryQueueNowFunc(clock.Now))
	ctx := t.Context()

	// Enqueue a delayed task first
	err := q.Enqueue(ctx, workflow.QueuedTask{
		InstanceID:   "wf-delayed",
		WorkflowName: "test",
		RunAfter:     clock.Now().Add(1 * time.Hour),
	})
	require.NoError(t, err)

	// Then an immediate task
	err = q.Enqueue(ctx, workflow.QueuedTask{
		InstanceID:   "wf-immediate",
		WorkflowName: "test",
	})
	require.NoError(t, err)

	// Poll should skip the delayed one and return the immediate one
	task, err := q.Poll(ctx)
	require.NoError(t, err)
	require.NotNil(t, task)
	require.Equal(t, workflow.InstanceID("wf-immediate"), task.InstanceID)

	// Delayed task is still there
	require.Equal(t, 1, q.Len())
}

func TestMemoryQueue_CompleteAndFail_AreNoOps(t *testing.T) {
	q := workflow.NewMemoryQueue()
	ctx := t.Context()

	// These should not panic or error even on unknown IDs
	require.NoError(t, q.Complete(ctx, "nonexistent"))
	require.NoError(t, q.Fail(ctx, "nonexistent"))
}

func TestMemoryQueue_ConcurrentAccess(t *testing.T) {
	q := workflow.NewMemoryQueue()
	ctx := t.Context()

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)

	// Concurrent enqueues
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_ = q.Enqueue(ctx, workflow.QueuedTask{
				InstanceID:   workflow.InstanceID(string(rune(i))),
				WorkflowName: "test",
			})
		}(i)
	}

	wg.Wait()
	require.Equal(t, n, q.Len())

	// Concurrent polls
	polled := make(chan *workflow.QueuedTask, n)
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			task, _ := q.Poll(ctx)
			if task != nil {
				polled <- task
			}
		}()
	}

	wg.Wait()
	close(polled)

	count := 0
	for range polled {
		count++
	}
	require.Equal(t, n, count, "every task should be polled exactly once")
	require.Equal(t, 0, q.Len())
}
