package ratelimit

import "time"

// Params configures a GCRA rate limit: Quota requests are allowed per
// Period, plus Burst additional requests tolerated in a single instant.
// Burst == 0 means strictly paced — no two requests can be admitted closer
// together than one emission interval, and the worst case admitted in any
// rolling window equal to Period is exactly Quota (see DESIGN-NOTES.md,
// "The worst-case rolling 60-second window", for the proof this
// implements). Burst == Quota-1 means a full quota's worth of requests can
// land in the same instant, then the limiter reverts to strict pacing.
type Params struct {
	Quota  int
	Period time.Duration
	Burst  int
}

// emissionInterval is the minimum spacing between admissions once burst
// tolerance is exhausted: one Quota-th of Period.
func (p Params) emissionInterval() time.Duration {
	return time.Duration(float64(p.Period) / float64(p.Quota))
}

// decide is the pure GCRA core. Given a customer's prior theoretical
// arrival time (tat), the arrival time of this request (now), and the
// rate parameters, it returns the decision and the TAT the caller should
// persist if it accepts this decision. It performs no I/O and reads no
// clock — now is supplied by the caller — so it is a plain deterministic
// function of its inputs and needs nothing more than a table of inputs to
// test exhaustively.
//
// The zero value of tat (time.Time{}) means "never seen this customer
// before." It is so far in the past relative to any real now that the
// admission check always passes, so a brand new customer's first request
// is always admitted without a separate bootstrap flag.
//
// On rejection, tat is returned unchanged: a rejected request must not
// consume any of the budget it was denied.
func decide(tat, now time.Time, p Params) (Decision, time.Time) {
	emission := p.emissionInterval()
	burstOffset := time.Duration(p.Burst) * emission

	// allowAt is the earliest instant at which a request would be
	// admitted, given the customer's current TAT and burst tolerance.
	allowAt := tat.Add(-burstOffset)

	if now.Before(allowAt) {
		return Decision{
			Allowed:    false,
			Remaining:  0,
			RetryAfter: allowAt.Sub(now), // allowAt is strictly after now here, so this is always > 0
			Limit:      p.Quota,
			Reason:     "rate_exceeded",
		}, tat
	}

	newTAT := tat
	if now.After(newTAT) {
		newTAT = now
	}
	newTAT = newTAT.Add(emission)

	// remaining: how many more requests could be admitted right now, at
	// this same instant. Each further admission would push newTAT forward
	// by one more emission interval; the number that still fit within
	// burstOffset of now is derived directly from that spacing, not
	// simulated by walking forward one call at a time.
	margin := newTAT.Sub(now)
	remaining := 0
	if margin <= burstOffset {
		remaining = int((burstOffset-margin)/emission) + 1
	}

	return Decision{
		Allowed:    true,
		Remaining:  remaining,
		RetryAfter: 0,
		Limit:      p.Quota,
		Reason:     "admitted",
	}, newTAT
}

// Limiter enforces a single GCRA rate limit across many customers, using a
// Clock supplied at construction so callers (and tests) control what time
// it is. Per-customer state lives behind a striped lock (store.go) so
// customers never contend with each other for the same mutex.
type Limiter struct {
	clock  Clock
	store  *store
	params Params
}

// NewLimiter returns a Limiter enforcing params, reading time from clock.
func NewLimiter(clock Clock, params Params) *Limiter {
	return &Limiter{clock: clock, store: newStore(), params: params}
}

// Allow decides whether customerID's next request is admitted right now,
// using the limiter's clock for the current time.
func (l *Limiter) Allow(customerID string) Decision {
	return l.AllowAt(customerID, l.clock.Now())
}

// AllowAt decides whether customerID's request arriving at now is
// admitted. Separated from Allow so a caller that already has an arrival
// timestamp (e.g. request receipt time in the HTTP layer, session 5)
// doesn't have to round-trip through the clock, and so tests can drive
// specific instants directly.
func (l *Limiter) AllowAt(customerID string, now time.Time) Decision {
	return l.store.withTAT(customerID, func(tat time.Time) (Decision, time.Time) {
		return decide(tat, now, l.params)
	})
}
