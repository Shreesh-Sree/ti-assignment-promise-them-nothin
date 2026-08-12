package policy

import (
	"log/slog"
	"sync/atomic"
	"time"

	"relayapi/internal/audit"
	"relayapi/internal/ratelimit"
)

// Decision is the result of resolving a customer's effective limit at a
// point in time.
type Decision struct {
	Limit  int
	Reason string // "tier_default", "override_applied", or "unknown_customer"
}

// Resolver answers exactly one question: given a customer ID and a
// timestamp, what limit applies, and why. It holds the current *Config
// behind an atomic pointer so Reload can swap in a new, already-validated
// config without a request ever observing a half-updated one, and without
// a restart.
type Resolver struct {
	cfg    atomic.Pointer[Config]
	clock  ratelimit.Clock
	logger *slog.Logger
}

// NewResolver loads and validates the config at path and returns a
// Resolver serving it. It returns an error — and the caller must not
// start serving traffic — if the config is invalid. Per DESIGN-NOTES.md:
// fail to start, don't warn.
func NewResolver(path string, clock ratelimit.Clock, logger *slog.Logger) (*Resolver, error) {
	r := &Resolver{clock: clock, logger: logger}
	if err := r.Reload(path); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload loads, parses, and validates the config at path, and only then
// swaps it in. A config that fails to load or fails validation is
// rejected and logged — the Resolver keeps serving whatever it last
// successfully loaded. This is the one code path both NewResolver and
// WatchSIGHUP use, so "starts with a bad config" and "reloads into a bad
// config" can't drift into two different bugs.
func (r *Resolver) Reload(path string) error {
	cfg, err := LoadConfig(path)
	if err != nil {
		return err
	}
	if err := cfg.Validate(r.clock.Now()); err != nil {
		return err
	}
	r.cfg.Store(cfg)
	return nil
}

// Resolve returns the effective limit for customerID at now, applying an
// override if — and only if — one is configured for this customer,
// currently within its window (plus grace), and not yet expired. now is
// an explicit argument, not read from a clock internally, so callers
// (including tests) control it directly with no clock plumbing required.
func (r *Resolver) Resolve(customerID string, now time.Time) Decision {
	cfg := r.cfg.Load()

	cust, tier, ok := cfg.lookup(customerID)
	if !ok {
		// No config entry for this customer: nothing to grant. What to do
		// about that (reject, fall back to a floor) is an httpapi
		// concern, not a policy one — this package only reports facts.
		return Decision{Limit: 0, Reason: "unknown_customer"}
	}
	contracted := contractedLimit(cust, tier)

	for _, o := range cfg.Overrides {
		if o.Customer != customerID || !o.activeAt(now) {
			continue
		}
		start, end := o.instantsFor(now)
		audit.OverrideApplied(r.logger, customerID, contracted, o.LimitRPM, o.Ticket, start, end)
		return Decision{Limit: o.LimitRPM, Reason: "override_applied"}
	}

	return Decision{Limit: contracted, Reason: "tier_default"}
}

// activeAt reports whether the override is in force at now: not expired,
// and now falls within its daily window plus grace.
func (o OverrideConfig) activeAt(now time.Time) bool {
	if !now.Before(o.expiresAt) {
		return false
	}
	start, end := o.instantsFor(now)
	return !now.Before(start) && now.Before(end)
}

// instantsFor resolves the override's recurring daily window to concrete
// instants for the UTC calendar date of now. end already includes
// GraceMinutes.
//
// Why grace exists at all: DESIGN-NOTES.md Part 1 §3 worked out that
// enforcing exactly the nominal 02:00-04:00 window has zero margin — a
// 120-minute batch starting exactly on time already ends exactly at the
// boundary, and the brief documents the start itself drifting with queue
// depth. Grace pads enforcement past the nominal end by an amount sized
// from that documented worst case: a 120-minute run, plus an assumed
// 60 minutes of queue-depth-driven start delay. That 60-minute figure is
// this system's own conservative assumption, not a number the brief
// gives — named here rather than buried in a config value with no
// explanation attached. It does not solve an unbounded-length batch; it
// converts a guaranteed-to-break, zero-margin cliff into one sized from
// the documented worst case, with the assumption it rests on visible.
func (o OverrideConfig) instantsFor(now time.Time) (start, end time.Time) {
	y, m, d := now.UTC().Date()
	startOfDay := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	startOffset, _ := parseTimeOfDay(o.Window.StartUTC) // already validated
	endOffset, _ := parseTimeOfDay(o.Window.EndUTC)     // already validated
	start = startOfDay.Add(startOffset)
	end = startOfDay.Add(endOffset).Add(time.Duration(o.Window.GraceMinutes) * time.Minute)
	return start, end
}
