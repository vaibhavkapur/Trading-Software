// Package clock lets every component take the time from one injectable
// source, so tests can control it.
package clock

import (
	"sync"
	"time"
)

// Clock returns the current time.
type Clock interface {
	Now() time.Time
}

// System is the wall clock, in UTC with microsecond precision. PostgreSQL
// stores microseconds, so truncating here keeps values identical before and
// after a round trip through the database.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Fake is a manually advanced clock.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake returns a clock frozen at t.
func NewFake(t time.Time) *Fake { return &Fake{now: t.UTC().Truncate(time.Microsecond)} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the clock forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
