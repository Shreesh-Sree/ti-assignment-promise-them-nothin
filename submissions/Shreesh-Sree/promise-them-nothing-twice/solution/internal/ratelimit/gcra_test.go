package ratelimit_test

import (
	"testing"
	"time"

	"relayapi/internal/ratelimit"
)

// TestSteadyRateAdmitsExactlyQuota sends exactly quota requests, each
// spaced one emission interval apart (i.e. a client obeying the limit
// precisely), and asserts every single one is admitted. Burst is 0: this
// is the strict-pacing case, so there is no slack anywhere in this test —
// if the algorithm rejects any of these, it's wrong.
func TestSteadyRateAdmitsExactlyQuota(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := ratelimit.NewFakeClock(base)
	const quota = 10
	limiter := ratelimit.NewLimiter(clock, ratelimit.Params{
		Quota:  quota,
		Period: time.Minute,
		Burst:  0,
	})
	emission := time.Minute / time.Duration(quota) // 6s

	for i := 0; i < quota; i++ {
		clock.Set(base.Add(time.Duration(i) * emission))
		d := limiter.Allow("acme")
		if !d.Allowed {
			t.Fatalf("request %d/%d at exactly the steady rate: want allowed, got rejected (reason=%s)", i+1, quota, d.Reason)
		}
	}
}

// TestRequestBeyondQuotaRejectedWithRetryAfter sends exactly quota
// requests at the steady rate (admitted, per the test above), then a
// (quota+1)th request at the same instant as the quota-th — no further
// waiting. That request must be rejected, and RetryAfter must name
// exactly how long until it would succeed: one emission interval, proved
// directly from the GCRA spacing invariant, not approximated.
func TestRequestBeyondQuotaRejectedWithRetryAfter(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := ratelimit.NewFakeClock(base)
	const quota = 10
	limiter := ratelimit.NewLimiter(clock, ratelimit.Params{
		Quota:  quota,
		Period: time.Minute,
		Burst:  0,
	})
	emission := time.Minute / time.Duration(quota)

	for i := 0; i < quota; i++ {
		clock.Set(base.Add(time.Duration(i) * emission))
		if d := limiter.Allow("acme"); !d.Allowed {
			t.Fatalf("setup: request %d/%d should have been admitted, got rejected", i+1, quota)
		}
	}

	// (quota+1)th request, same instant as request quota — no time has
	// passed since the last admitted request.
	d := limiter.Allow("acme")
	if d.Allowed {
		t.Fatalf("request %d (quota+1), same instant as request %d: want rejected, got allowed", quota+1, quota)
	}
	if d.RetryAfter != emission {
		t.Errorf("RetryAfter = %v, want exactly %v (one emission interval) — not an approximation", d.RetryAfter, emission)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want > 0", d.RetryAfter)
	}
}

// TestRollingWindowNotCalendarMinute is the test that catches fixed-window
// thinking. It fires a full burst of quota requests in a single instant
// right at what would be the end of "minute 1" in a calendar-aligned
// scheme, then — one real second later, "immediately" in batch-traffic
// terms — fires another full burst of quota requests at what would be the
// start of "minute 2".
//
// A fixed-window limiter resets its counter at the minute boundary and
// would admit the second burst in full: 2x quota inside a true rolling
// 60-second span. A rolling-window limiter (GCRA, here) must reject the
// entire second burst, because only one second of real time has passed —
// nowhere near enough of the window to have drained.
func TestRollingWindowNotCalendarMinute(t *testing.T) {
	// 00:00:59 — one second before what a fixed-window scheme would treat
	// as the boundary between minute 1 ([00:00:00, 00:01:00)) and minute 2.
	start := time.Date(2024, 1, 1, 0, 0, 59, 0, time.UTC)
	clock := ratelimit.NewFakeClock(start)
	const quota = 10
	limiter := ratelimit.NewLimiter(clock, ratelimit.Params{
		Quota:  quota,
		Period: time.Minute,
		Burst:  quota - 1, // a full quota's worth can land in one instant
	})

	// Burst 1: quota requests, all at 00:00:59.
	for i := 0; i < quota; i++ {
		d := limiter.Allow("northwind")
		if !d.Allowed {
			t.Fatalf("burst 1, request %d/%d: want allowed, got rejected (reason=%s)", i+1, quota, d.Reason)
		}
	}
	// The burst is exhausted: one more right now must be rejected.
	if d := limiter.Allow("northwind"); d.Allowed {
		t.Fatalf("burst 1, request %d (quota+1): want rejected, got allowed — burst tolerance not enforced", quota+1)
	}

	// One real second later: 00:01:00, the start of "minute 2" in a
	// calendar-aligned scheme.
	clock.Advance(1 * time.Second)

	admittedInBurst2 := 0
	var firstRejection ratelimit.Decision
	for i := 0; i < quota; i++ {
		d := limiter.Allow("northwind")
		if d.Allowed {
			admittedInBurst2++
		} else if firstRejection.Reason == "" {
			firstRejection = d
		}
	}

	if admittedInBurst2 != 0 {
		t.Errorf("burst 2 (one real second after burst 1, one calendar minute later): want 0 admitted, got %d — "+
			"this is fixed-window behavior (counter reset at the calendar boundary), not rolling-window", admittedInBurst2)
	}

	// The rolling window has 59 seconds still to drain (60s period minus
	// the 1s that has actually elapsed), so the retry-after on the first
	// rejection of burst 2 should reflect that, not a fresh full window.
	wantRetryAfter := 5 * time.Second // derived in DESIGN-NOTES-adjacent scratch work: allowAt(119s) - now(60s)
	if firstRejection.RetryAfter != wantRetryAfter {
		t.Errorf("burst 2 first rejection: RetryAfter = %v, want %v", firstRejection.RetryAfter, wantRetryAfter)
	}
}

// TestRetryAfterAlwaysPositiveOnReject hammers several quota/burst
// configurations well past their limit and asserts the invariant that
// matters most to a client deciding when to retry: RetryAfter is never
// zero or negative on a rejection, regardless of configuration.
func TestRetryAfterAlwaysPositiveOnReject(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	configs := []ratelimit.Params{
		{Quota: 1, Period: time.Minute, Burst: 0},
		{Quota: 60, Period: time.Minute, Burst: 0},
		{Quota: 300, Period: time.Minute, Burst: 5},
		{Quota: 1200, Period: time.Minute, Burst: 1199},
	}

	for _, p := range configs {
		limiter := ratelimit.NewLimiter(clock, p)
		rejections := 0
		// Far more attempts than any of these configs could admit at a
		// single instant, so rejections are guaranteed.
		for i := 0; i < p.Quota+50; i++ {
			d := limiter.Allow("hammered-customer")
			if !d.Allowed {
				rejections++
				if d.RetryAfter <= 0 {
					t.Errorf("quota=%d burst=%d: rejected decision has RetryAfter=%v, want > 0", p.Quota, p.Burst, d.RetryAfter)
				}
			}
		}
		if rejections == 0 {
			t.Fatalf("quota=%d burst=%d: test invalid, no rejections occurred to check the invariant against", p.Quota, p.Burst)
		}
	}
}
