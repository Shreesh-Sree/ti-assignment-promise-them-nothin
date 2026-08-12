package ratelimit_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"relayapi/internal/ratelimit"
)

// TestTwoCustomersIsolatedUnderConcurrency hammers a single shared Limiter
// with two customers at once, from many goroutines, with deliberately
// unequal attempt counts (250 vs 400) so that asymmetric contention would
// show up if the two customers' state leaked into each other. Run with
// -race: the striped lock in store.go is the thing being tested here, and
// a race detector catching a data race is as much a failure as a wrong
// count.
func TestTwoCustomersIsolatedUnderConcurrency(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	const quota = 100
	limiter := ratelimit.NewLimiter(clock, ratelimit.Params{
		Quota:  quota,
		Period: time.Minute,
		Burst:  quota - 1, // let a full quota land in one instant, since every goroutine fires at the same fake "now"
	})

	const attemptsA = 250
	const attemptsB = 400

	var admittedA, admittedB int64
	var wg sync.WaitGroup

	wg.Add(attemptsA + attemptsB)
	for i := 0; i < attemptsA; i++ {
		go func() {
			defer wg.Done()
			if limiter.Allow("customer-a").Allowed {
				atomic.AddInt64(&admittedA, 1)
			}
		}()
	}
	for i := 0; i < attemptsB; i++ {
		go func() {
			defer wg.Done()
			if limiter.Allow("customer-b").Allowed {
				atomic.AddInt64(&admittedB, 1)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&admittedA); got != quota {
		t.Errorf("customer-a: want exactly %d admitted out of %d concurrent attempts (customer-b contending throughout), got %d",
			quota, attemptsA, got)
	}
	if got := atomic.LoadInt64(&admittedB); got != quota {
		t.Errorf("customer-b: want exactly %d admitted out of %d concurrent attempts (customer-a contending throughout), got %d",
			quota, attemptsB, got)
	}
}
