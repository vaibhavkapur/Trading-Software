package types

import (
	"sync"
	"time"
)

// Clock provides UTC timestamps and supports deterministic testing and replay.
type Clock interface {
	Now() time.Time
}

// SystemClock returns the current wall-clock time in UTC.
type SystemClock struct{}

func (SystemClock) Now() time.Time {
	return time.Now().UTC()
}

// FixedClock is a thread-safe deterministic clock for unit tests, replay, and clock-skew scenarios.
type FixedClock struct {
	mu  sync.RWMutex
	now time.Time
}

func NewFixedClock(t time.Time) *FixedClock {
	return &FixedClock{now: t.UTC()}
}

func (c *FixedClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now.UTC()
}

func (c *FixedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t.UTC()
}

func (c *FixedClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d).UTC()
	return c.now
}
