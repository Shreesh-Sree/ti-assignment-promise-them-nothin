// Package ratelimit implements the metering core described in
// solution/DESIGN-NOTES.md: a GCRA rate limiter, exact per the proof in
// that document, with per-customer state isolated behind a striped lock.
//
// This package is single-node and has no knowledge of coordination,
// config, or HTTP. Those live in internal/coordinator, internal/policy,
// and internal/httpapi respectively, built in later sessions.
package ratelimit

import (
	"sync"
	"time"
)

// Clock supplies the current time to the limiter. Production code uses
// RealClock. Every test in this package uses FakeClock instead, so time
// only ever moves when a test explicitly moves it — no time.Sleep, no
// flakiness tied to how fast the test happens to run.
type Clock interface {
	Now() time.Time
}

// RealClock reads the system clock.
type RealClock struct{}

// Now returns the current wall-clock time.
func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a manually driven clock for tests. The zero value is not
// usable; construct one with NewFakeClock. Safe for concurrent use, since
// tests exercise the limiter from multiple goroutines while the clock is
// held fixed.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a FakeClock starting at now.
func NewFakeClock(now time.Time) *FakeClock {
	return &FakeClock{now: now}
}

// Now returns the clock's current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set moves the clock to an absolute time. Tests use this when it's
// clearer to state the instant a request arrives at directly, rather than
// accumulate it via a sequence of Advance calls.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
