// Package audit emits the structured events DESIGN-NOTES.md's audit
// requirement names. It has no state and no dependency on policy or
// ratelimit — it's a thin, typed layer over log/slog so the shape of an
// audit event is enforced by the compiler rather than by convention.
package audit

import (
	"log/slog"
	"time"
)

// OverrideApplied is emitted every time — and only when — an override
// changes a customer's effective limit away from their contracted tier
// limit. It's a typed function, not a formatted string: every field the
// audit requirement names (customer, contracted limit, effective limit,
// ticket, window) is a required parameter, so a call site can't
// accidentally omit one the way it could with a hand-built log line.
func OverrideApplied(logger *slog.Logger, customerID string, contractedLimitRPM, effectiveLimitRPM int, ticket string, windowStart, windowEnd time.Time) {
	logger.Info("override_applied",
		slog.String("event", "override_applied"),
		slog.String("customer_id", customerID),
		slog.Int("contracted_limit_rpm", contractedLimitRPM),
		slog.Int("effective_limit_rpm", effectiveLimitRPM),
		slog.String("override_ticket", ticket),
		slog.Time("window_start", windowStart),
		slog.Time("window_end", windowEnd),
	)
}
