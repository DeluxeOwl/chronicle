package workflow

import (
	"context"
	"sync"
	"time"
)

// MemoryQueue is a simple in-memory TaskQueue backed by a mutex-protected
// slice.  Suitable for single-process use and testing.
type MemoryQueue struct {
	mu      sync.Mutex
	tasks   []QueuedTask
	nowFunc func() time.Time
}

// MemoryQueueOption configures a MemoryQueue.
type MemoryQueueOption func(*MemoryQueue)

// WithMemoryQueueNowFunc sets a custom clock for the queue.
// Defaults to time.Now.
func WithMemoryQueueNowFunc(fn func() time.Time) MemoryQueueOption {
	return func(q *MemoryQueue) {
		q.nowFunc = fn
	}
}

// NewMemoryQueue creates a new in-memory task queue.
func NewMemoryQueue(opts ...MemoryQueueOption) *MemoryQueue {
	q := &MemoryQueue{
		nowFunc: time.Now,
	}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

func (q *MemoryQueue) Enqueue(_ context.Context, task QueuedTask) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.tasks = append(q.tasks, task)
	return nil
}

func (q *MemoryQueue) Poll(_ context.Context) (*QueuedTask, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := q.nowFunc()

	for i, task := range q.tasks {
		if !task.RunAfter.IsZero() && now.Before(task.RunAfter) {
			continue // not ready yet
		}
		// Remove from slice (order-preserving)
		q.tasks = append(q.tasks[:i], q.tasks[i+1:]...)
		return &task, nil
	}

	return nil, nil
}

func (q *MemoryQueue) Complete(_ context.Context, _ InstanceID) error {
	// In the memory queue, Poll already removed the task. Nothing to do.
	return nil
}

func (q *MemoryQueue) Fail(_ context.Context, _ InstanceID) error {
	// In the memory queue, Poll already removed the task.
	// A real implementation would move to a dead-letter or re-enqueue with backoff.
	return nil
}

// Len returns the number of tasks in the queue (useful for testing).
func (q *MemoryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

// Compile-time interface check.
var _ TaskQueue = (*MemoryQueue)(nil)
