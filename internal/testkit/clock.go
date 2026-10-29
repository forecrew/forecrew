package testkit

import (
	"sync"
	"time"
)

// Clock is a manual clock for fakes. The zero value is not usable; use
// NewClock.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock returns a clock set to t (UTC).
func NewClock(t time.Time) *Clock {
	return &Clock{now: t.UTC()}
}

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// realNow is the default clock of fakes.
func realNow() time.Time { return time.Now().UTC() }

func orNow(now func() time.Time) func() time.Time {
	if now == nil {
		return realNow
	}
	return now
}
