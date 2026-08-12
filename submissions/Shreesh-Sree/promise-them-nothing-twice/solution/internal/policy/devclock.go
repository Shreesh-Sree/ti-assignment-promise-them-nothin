package policy

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"relayapi/internal/ratelimit"
)

// EnvDevClockAsOf is the environment variable that, if set, shifts the
// process's notion of "now" for as long as it runs — the mechanism this
// system provides for exercising Northwind's nightly window (or any
// other time-boxed override) live, in the harness or a manual demo,
// without waiting for real clock time to reach 02:00 UTC.
//
// It is off by default: unset, NewClockFromEnv returns
// ratelimit.RealClock unmodified and does nothing else — no parsing, no
// log line, no behavior change. Deliberately not a header, query
// parameter, or request body field: nothing in an HTTP request can
// influence it. It is read once, from the process's own environment, at
// startup, by whoever controls how that process is launched — a
// fundamentally different trust boundary than "anything a client can
// send," which is the property that makes this safe to build at all.
//
// Risk if this ships enabled in a real deployment: the process's clock
// silently and uniformly diverges from real time for every request it
// handles, for as long as it keeps running. That's not cosmetic here —
// DailyWindow.activeAt and the expiry check both read straight from this
// clock, so a stuck or forgotten override tells Northwind's override (or
// any override) to be active far longer than its real window, or makes
// an already-expired override still look current. It fails in exactly
// the "quietly permanent" direction the mandatory-expiry rule in
// DESIGN-NOTES.md Part 1 §2 exists to prevent, just via a different
// mechanism — a clock bug instead of a missing expiry field. Nothing in
// this package wires it into a real binary; that's cmd/relayapi's job, in
// a later session, and whoever does that wiring is responsible for making
// it impossible to set by accident — e.g. never sourced from a shared
// staging env file that could be copied into a production one, and never
// set anywhere near the customers.yaml config path this same process
// reads, so a reviewer auditing overrides never has to also audit this.
const EnvDevClockAsOf = "RELAYAPI_DEV_CLOCK_AS_OF"

// NewClockFromEnv returns ratelimit.RealClock unless EnvDevClockAsOf is
// set, in which case it returns a clock that believes the current instant
// — as of the moment this function was called — was the given RFC3339
// timestamp, and continues to advance at normal real-time speed from
// there. Time still flows (a demo can watch the override window open and
// close), it's just shifted, computed once at startup.
//
// A malformed value panics rather than silently falling back to the real
// clock: a typo here should be impossible to miss, not something that
// looks like nothing happened.
func NewClockFromEnv(logger *slog.Logger) ratelimit.Clock {
	val, ok := os.LookupEnv(EnvDevClockAsOf)
	if !ok || val == "" {
		return ratelimit.RealClock{}
	}

	target, err := time.Parse(time.RFC3339, val)
	if err != nil {
		panic(fmt.Sprintf("policy: %s is set but not a valid RFC3339 timestamp: %v", EnvDevClockAsOf, err))
	}

	offset := time.Until(target)
	logger.Warn("DEV CLOCK OVERRIDE ACTIVE — this process's clock is shifted and does not reflect real time. Never set in production.",
		"env_var", EnvDevClockAsOf,
		"as_of", target,
		"offset", offset,
	)
	return offsetClock{offset: offset}
}

// offsetClock reads the real clock and applies a fixed offset, computed
// once when NewClockFromEnv was called.
type offsetClock struct {
	offset time.Duration
}

func (c offsetClock) Now() time.Time { return time.Now().Add(c.offset) }
