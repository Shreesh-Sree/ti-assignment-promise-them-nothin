package ratelimit

import "time"

// Decision is the outcome of a single admission check.
//
// Reason is populated on every decision, not only rejections, because the
// audit trail (internal/audit, session 4) needs to record why a request
// was allowed too — for example, which override applied and why. Putting
// it in now avoids threading a new field through every caller later.
type Decision struct {
	// Allowed reports whether the request may proceed.
	Allowed bool

	// Remaining is how many additional requests could be admitted for this
	// customer at the same instant as this decision, given the limit that
	// was applied. It is always 0 on a rejection.
	Remaining int

	// RetryAfter is how long the customer must wait before a retry could
	// succeed. It is always > 0 when Allowed is false, and always 0 when
	// Allowed is true.
	RetryAfter time.Duration

	// Limit is the quota (requests per period) that was applied to reach
	// this decision.
	Limit int

	// Reason is a short, stable machine-readable string explaining the
	// decision, e.g. "admitted" or "rate_exceeded". Later sessions widen
	// this set (e.g. an override-specific reason) without changing the
	// shape of Decision itself.
	Reason string
}
