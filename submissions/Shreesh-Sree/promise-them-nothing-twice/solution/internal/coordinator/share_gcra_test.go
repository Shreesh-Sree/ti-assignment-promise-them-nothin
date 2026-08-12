package coordinator

import (
	"testing"
	"time"
)

// TestShareStateSteadyRateAdmitsExactlyQuota mirrors
// ratelimit.TestSteadyRateAdmitsExactlyQuota exactly, as a check that this
// package's reimplementation of the GCRA formula agrees with the one it's
// deliberately not allowed to import mutability into.
func TestShareStateSteadyRateAdmitsExactlyQuota(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const quota = 10
	s := newShareState(quota)
	period := time.Minute
	emission := period / time.Duration(quota)

	for i := range quota {
		now := base.Add(time.Duration(i) * emission)
		d := s.allow(now, period)
		if !d.Allowed {
			t.Fatalf("request %d/%d at exactly the steady rate: want allowed, got rejected", i+1, quota)
		}
	}
}

// TestShareStateRejectsBeyondQuotaAtSameInstant mirrors
// ratelimit.TestRequestBeyondQuotaRejectedWithRetryAfter, extended for the
// adopted Burst=1 tolerance (DECISIONS.md's Burst tradeoff): one extra
// request at the same instant as the quota-th is now admitted — that's
// what Burst=1 buys — and only the request after THAT is rejected.
// Verified against newShareState's actual computed output, not hand
// arithmetic: at these quota=10 values the numbers happen to work out so
// RetryAfter on the truly-rejected request is still exactly one emission
// interval, same as the Burst=0 case, but that's this specific quota's
// arithmetic lining up, not a general property — see
// TestShareStateShrinkNeverOverAdmits below for a case where Burst=1's
// effect is much larger than "+1" and isn't a coincidence-free number.
func TestShareStateRejectsBeyondQuotaAtSameInstant(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const quota = 10
	s := newShareState(quota)
	period := time.Minute
	emission := period / time.Duration(quota)

	var last time.Time
	for i := range quota {
		last = base.Add(time.Duration(i) * emission)
		if d := s.allow(last, period); !d.Allowed {
			t.Fatalf("setup: request %d/%d should have been admitted", i+1, quota)
		}
	}

	// (quota+1)th request, same instant: the one request of Burst=1
	// tolerance — admitted, not rejected.
	if d := s.allow(last, period); !d.Allowed {
		t.Fatalf("request %d (quota+1), same instant as request %d: want allowed (Burst=1 tolerance), got rejected", quota+1, quota)
	}

	// (quota+2)th request, still the same instant: tolerance is spent.
	d := s.allow(last, period)
	if d.Allowed {
		t.Fatalf("request %d (quota+2), same instant: want rejected (tolerance exhausted), got allowed", quota+2)
	}
	if d.RetryAfter != emission {
		t.Errorf("RetryAfter = %v, want exactly %v", d.RetryAfter, emission)
	}
}

// TestShareStateSetQuotaPreservesTAT is the property the whole peer
// coordinator design depends on: changing Quota mid-stream must not reset
// TAT. A customer paced at quota=10 who then gets grown to quota=20 must
// not suddenly be able to burst a fresh quota's worth of admissions — the
// spacing already earned under the old quota still applies going forward,
// just at the new (looser) emission interval.
//
// With Burst=1 adopted (DECISIONS.md's Burst tradeoff), the boundary
// right at the drain's last admission now tolerates one extra request —
// that's tested and spent explicitly in its own labeled step below, so
// the rest of this test (the actual TAT-preservation property) isn't
// entangled with burst-edge arithmetic. Every value here was checked
// against newShareState's real computed output, not derived by hand —
// growing quota does NOT reset TAT to something permissive is the one
// property this test exists to prove, and it holds.
func TestShareStateSetQuotaPreservesTAT(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newShareState(10)
	period := time.Minute

	// Drain the quota=10 pacing at the steady rate. TAT ends at 60s.
	emission10 := period / 10
	var last time.Time
	for i := range 10 {
		last = base.Add(time.Duration(i) * emission10)
		if d := s.allow(last, period); !d.Allowed {
			t.Fatalf("setup: request %d/10 should have been admitted", i+1)
		}
	}

	// Spend the Burst=1 tolerance explicitly, at the same instant as
	// `last` (54s): admitted once (TAT advances to 66s), then a second
	// call at the same instant is rejected (tolerance now spent). This is
	// setup for what follows, not the property under test.
	if d := s.allow(last, period); !d.Allowed {
		t.Fatalf("setup: expected the Burst=1 tolerance request to be admitted")
	}
	if d := s.allow(last, period); d.Allowed {
		t.Fatalf("setup: expected rejection once the Burst=1 tolerance is spent, got allowed")
	}

	// Grow to quota=20. TAT is now 66s (not the original drain's 60s —
	// the tolerance step above legitimately advanced it further). If TAT
	// were reset instead of preserved, every check below would wrongly
	// admit, since a fresh TAT is "never seen before".
	s.setQuota(20)
	if d := s.allow(last, period); d.Allowed {
		t.Fatalf("request at same instant as last admission, immediately after growing quota: want rejected (TAT must carry forward), got allowed")
	}

	// A request paced one full OLD emission interval after `last` (60s)
	// is still rejected — this is the sharpest proof TAT carried forward:
	// a bug that reset TAT on setQuota would admit this immediately
	// (fresh TAT + new, faster quota=20 cadence), but the real TAT (66s)
	// with the new burstOffset (1 x 3s = 3s) puts allowAt at 63s, still
	// ahead of this 60s request.
	oldCadenceNext := last.Add(emission10)
	if d := s.allow(oldCadenceNext, period); d.Allowed {
		t.Fatalf("request at last+emission10 (60s), right after growing quota: want rejected (TAT=66s must carry forward, not reset), got allowed")
	}

	// A request 3s later (63s = TAT(66s) - new burstOffset(3s)) is
	// exactly where the new, looser quota=20 cadence becomes admissible —
	// proving the new quota is live, without re-testing burst-edge
	// precision (already covered by the dedicated burst tests).
	emission20 := period / 20
	newCadenceAdmit := oldCadenceNext.Add(emission20)
	if d := s.allow(newCadenceAdmit, period); !d.Allowed {
		t.Errorf("request at last+emission10+emission20 (63s): want allowed (new quota=20 cadence now live), got rejected")
	}
}

// TestShareStateShrinkNeverOverAdmits checks the other direction. A shrink
// does not retroactively revoke slots pacing has already earned — there
// is nothing to revoke, allow() never looks backward — but it does widen
// every emission interval computed AFTER those slots, so the customer
// converges to the new, smaller rate, never sustaining the old faster one
// indefinitely.
//
// With Burst=1 adopted, this test's numbers (quota 20 shrinking to 10,
// a first-ever admission whose TAT starts only one emission interval
// ahead of "now") land in a regime where the new quota's burstOffset
// (1 x 6s = 6s) is WIDE relative to how little time has passed — the
// effect is not a clean "+1 request" the way TestStaticSplitsEvenly's
// larger, steady-state numbers show it. It's genuinely two extra
// admissions at the fixed instant below before a rejection appears, and
// that number came from running newShareState itself (see this session's
// scratch verification), not from assuming Burst=1 always means "+1" —
// it doesn't; the exact count depends on how close TAT already was to
// "now" when the burstOffset is computed. The property under test — the
// customer can't sustain the OLD faster cadence forever after a shrink —
// still holds; it just takes one call longer to observe here than a
// same-quota-throughout intuition would suggest.
func TestShareStateShrinkNeverOverAdmits(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	s := newShareState(20)
	period := time.Minute
	emission20 := period / 20

	last := base
	if d := s.allow(last, period); !d.Allowed {
		t.Fatalf("setup: first request should be admitted")
	}
	// TAT now sits at last + emission20 (3s) — a slot already priced in
	// under the old quota, before any shrink happened.

	s.setQuota(10) // shrink
	emission10 := period / 10

	// The already-earned slot (t0 = 3s) is still honored...
	t0 := last.Add(emission20)
	if d := s.allow(t0, period); !d.Allowed {
		t.Fatalf("request at the TAT already earned before the shrink: want allowed, got rejected")
	}

	// ...and at this quota/timing combination, the new quota=10
	// burstOffset (6s) is wide enough that a SECOND request at the same
	// instant t0 is also admitted — real computed behavior, not a
	// hand-derived guess.
	if d := s.allow(t0, period); !d.Allowed {
		t.Fatalf("second request at t0 (3s): want allowed (new quota's wide burstOffset relative to elapsed time), got rejected")
	}

	// A third request at t0 is where the tolerance is finally spent:
	// rejected, with RetryAfter reflecting the new, stricter emission10
	// spacing now in force.
	d := s.allow(t0, period)
	if d.Allowed {
		t.Errorf("third request at t0: want rejected (shrink's tolerance now exhausted), got allowed")
	}

	// Only pacing at the new, stricter interval from here is admitted —
	// the customer has converged to the smaller quota.
	onTime := t0.Add(emission10)
	if d := s.allow(onTime, period); !d.Allowed {
		t.Errorf("request paced at the new, stricter interval after a shrink: want allowed, got rejected")
	}
}
