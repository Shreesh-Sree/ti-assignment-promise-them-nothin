package coordinator_test

import (
	"testing"
	"time"

	"relayapi/internal/coordinator"
	"relayapi/internal/ratelimit"
)

// TestStaticSplitsEvenly checks the defining property of the naive
// strategy: a customer's per-node share is exactly globalLimit/nodeCount,
// enforced with GCRA pacing at the adopted Burst=1 tolerance (DECISIONS.md's
// Burst tradeoff) — sending exactly share requests at the steady rate must
// all be admitted, one more at the same instant as the last is also
// admitted (that's the one request of tolerance Burst=1 buys), and only
// the request after THAT is rejected. This is deliberately the same shape
// as ratelimit's own TestSteadyRateAdmitsExactlyQuota, one layer up:
// coordinator's job is only to pick the right Quota (and now Burst), not
// to reimplement pacing correctness, which ratelimit already proves.
//
// Before the Burst tradeoff was adopted, this test asserted the (share+1)th
// request was rejected — that assertion depended on the exact Burst=0
// value and broke the moment NodeBurst became 1, exactly as expected: it
// was testing the boundary Burst controls, not an incidental detail.
func TestStaticSplitsEvenly(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := ratelimit.NewFakeClock(base)
	c := coordinator.NewStatic("node-1", 3, clock)

	const globalLimit = 300
	const wantShare = 100 // 300 / 3 nodes
	emission := time.Minute / time.Duration(wantShare)

	now := base
	for i := range wantShare {
		now = base.Add(time.Duration(i) * emission)
		d := c.Allow("cust", globalLimit, now)
		if !d.Allowed {
			t.Fatalf("request %d/%d at exactly the node's steady share rate: want allowed, got rejected", i+1, wantShare)
		}
		if d.Limit != wantShare {
			t.Fatalf("Decision.Limit = %d, want %d (the node's share, not the global limit %d)", d.Limit, wantShare, globalLimit)
		}
	}

	// (share+1)th request, same instant as the last admitted one: this is
	// the single request of Burst=1 tolerance — admitted, not rejected.
	if d := c.Allow("cust", globalLimit, now); !d.Allowed {
		t.Fatalf("request %d, same instant as request %d: want allowed (Burst=1 tolerance), got rejected", wantShare+1, wantShare)
	}

	// (share+2)th request, still the same instant: tolerance is spent —
	// this one must be rejected.
	d := c.Allow("cust", globalLimit, now)
	if d.Allowed {
		t.Fatalf("request %d, same instant as request %d: want rejected (share and Burst=1 tolerance both exhausted), got allowed", wantShare+2, wantShare)
	}
}

// TestStaticBurstAtSameInstantCappedAtShare fires a wall of requests at
// the same instant and checks the node admits exactly Burst+1 — not the
// full share, not the full global limit, and not exactly one either now
// that Burst=1 is the adopted value (DECISIONS.md's Burst tradeoff).
// Confirms coordinator passes NodeBurst through as documented in
// static.go, rather than silently defaulting to something looser or
// reverting to strict Burst=0.
func TestStaticBurstAtSameInstantCappedAtShare(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	c := coordinator.NewStatic("node-1", 3, clock)

	admitted := 0
	for range 1000 {
		if c.Allow("cust", 300, clock.Now()).Allowed {
			admitted++
		}
	}
	const wantAdmitted = 2 // Burst=1 tolerates exactly one extra admission at the same instant, on top of the first: 1 (base) + 1 (tolerance) = 2
	if admitted != wantAdmitted {
		t.Errorf("admitted %d requests in a single instant with Burst=1; want exactly %d", admitted, wantAdmitted)
	}
}

// TestStaticQuotaStateReportsShare exercises the /internal/quota-state
// contract at the coordinator level: after at least one request for a
// customer, QuotaState must report that customer's node share.
func TestStaticQuotaStateReportsShare(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	c := coordinator.NewStatic("node-1", 3, clock)
	c.Allow("cust_a", 300, clock.Now())

	state := c.QuotaState()
	if state.Mode != "static" {
		t.Errorf("Mode = %q, want %q", state.Mode, "static")
	}
	if len(state.Shares) != 1 {
		t.Fatalf("Shares = %v, want exactly one entry after one customer's first request", state.Shares)
	}
	if state.Shares[0].NodeShare != 100 {
		t.Errorf("NodeShare = %d, want 100", state.Shares[0].NodeShare)
	}
}

// TestStaticRoundsShareUp checks the documented rounding direction: a
// global limit that doesn't divide evenly across nodeCount rounds each
// node's share UP, so the sum of shares is never less than the global
// limit — biasing any slack toward admitting, never toward under-serving
// every node below its true fraction.
func TestStaticRoundsShareUp(t *testing.T) {
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	c := coordinator.NewStatic("node-1", 3, clock)

	c.Allow("cust", 100, clock.Now()) // 100 / 3 = 33.33...

	state := c.QuotaState()
	if got := state.Shares[0].NodeShare; got != 34 {
		t.Errorf("NodeShare = %d, want 34 (ceil(100/3))", got)
	}
}
