package workflow

import (
	"context"
	"slices"
	"sync"
	"time"
)

// MemoryQueue is an in-process task queue. RunWorkers uses recoverable claims;
// the legacy Poll method retains its destructive, lease-free behavior.
// Tasks do not survive a process restart.
type MemoryQueue struct {
	mu      sync.Mutex
	tasks   []QueuedTask
	claims  map[InstanceID]memoryClaim
	nowFunc func() time.Time
}

type memoryClaim struct {
	task      ClaimedTask
	expiresAt time.Time
	next      *QueuedTask
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
		mu:      sync.Mutex{},
		tasks:   []QueuedTask{},
		claims:  make(map[InstanceID]memoryClaim),
	}
	for _, opt := range opts {
		opt(q)
	}
	return q
}

func (q *MemoryQueue) Enqueue(ctx context.Context, task QueuedTask) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	if claim, ok := q.claims[task.InstanceID]; ok {
		if claim.next == nil || task.RunAfter.Before(claim.next.RunAfter) ||
			task.RunAfter.IsZero() {
			claim.next = &task
			q.claims[task.InstanceID] = claim
		}
		return nil
	}
	q.tasks = append(q.tasks, task)
	return nil
}

func (q *MemoryQueue) Poll(ctx context.Context) (*QueuedTask, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.recoverExpired(q.nowFunc())
	return q.takeReady(), nil
}

func (q *MemoryQueue) takeReady() *QueuedTask {
	now := q.nowFunc()
	for i, task := range q.tasks {
		if _, claimed := q.claims[task.InstanceID]; claimed {
			continue
		}
		if !task.RunAfter.IsZero() && now.Before(task.RunAfter) {
			continue
		}
		q.tasks = slices.Delete(q.tasks, i, i+1)
		return &task
	}
	return nil
}

func (q *MemoryQueue) PollClaim(ctx context.Context) (*ClaimedTask, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := q.nowFunc()
	q.recoverExpired(now)
	task := q.takeReady()
	if task == nil {
		//nolint:nilnil // An empty queue is not an error.
		return nil, nil
	}
	claimed := ClaimedTask{QueuedTask: *task, Receipt: newClaimReceipt()}
	q.claims[task.InstanceID] = memoryClaim{
		task:      claimed,
		expiresAt: now.Add(defaultQueueLeaseDuration),
		next:      nil,
	}
	return &claimed, nil
}

func (q *MemoryQueue) recoverExpired(now time.Time) {
	for id, claim := range q.claims {
		if now.Before(claim.expiresAt) {
			continue
		}
		if claim.next != nil {
			q.tasks = append(q.tasks, *claim.next)
		} else {
			q.tasks = append(q.tasks, claim.task.QueuedTask)
		}
		delete(q.claims, id)
	}
}

func (q *MemoryQueue) ownedClaim(task ClaimedTask) (memoryClaim, error) {
	claim, ok := q.claims[task.InstanceID]
	if !ok || task.Receipt == "" || claim.task.Receipt != task.Receipt ||
		!q.nowFunc().Before(claim.expiresAt) {
		return memoryClaim{}, ErrStaleClaim
	}
	return claim, nil
}

func (q *MemoryQueue) CompleteClaim(ctx context.Context, task ClaimedTask) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	claim, err := q.ownedClaim(task)
	if err != nil {
		return err
	}
	if claim.next != nil {
		q.tasks = append(q.tasks, *claim.next)
	}
	delete(q.claims, task.InstanceID)
	return nil
}

func (q *MemoryQueue) FailClaim(ctx context.Context, task ClaimedTask) error {
	return q.CompleteClaim(ctx, task)
}

func (q *MemoryQueue) ExtendClaim(
	ctx context.Context,
	task ClaimedTask,
	lease time.Duration,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateLease(lease); err != nil {
		return err
	}
	claim, err := q.ownedClaim(task)
	if err != nil {
		return err
	}
	claim.expiresAt = laterDeadline(claim.expiresAt, q.nowFunc().Add(lease))
	q.claims[task.InstanceID] = claim
	return nil
}

func (q *MemoryQueue) ReleaseClaim(
	ctx context.Context,
	task ClaimedTask,
	runAfter time.Time,
) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	claim, err := q.ownedClaim(task)
	if err != nil {
		return err
	}
	if claim.next != nil {
		q.tasks = append(q.tasks, *claim.next)
	} else {
		retry := claim.task.QueuedTask
		retry.RunAfter = runAfter
		q.tasks = append(q.tasks, retry)
	}
	delete(q.claims, task.InstanceID)
	return nil
}

func (q *MemoryQueue) claimLeaseDuration() time.Duration { return defaultQueueLeaseDuration }

func (q *MemoryQueue) Complete(_ context.Context, _ InstanceID) error { return nil }

func (q *MemoryQueue) Fail(_ context.Context, _ InstanceID) error { return nil }

func (q *MemoryQueue) ExtendLease(
	_ context.Context,
	_ InstanceID,
	_ time.Duration,
) error {
	return nil
}

// Len returns the number of queued tasks, excluding active claims.
func (q *MemoryQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

var (
	_ ClaimedTaskQueue    = (*MemoryQueue)(nil)
	_ ClaimedTaskReleaser = (*MemoryQueue)(nil)
)
