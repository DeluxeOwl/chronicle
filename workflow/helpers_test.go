package workflow_test

import (
	"sync"
	"time"
)

// controllableClock provides a clock that can be advanced manually.
// All reads and writes are synchronized.
type controllableClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *controllableClock {
	return &controllableClock{now: t}
}

func (c *controllableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controllableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *controllableClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
