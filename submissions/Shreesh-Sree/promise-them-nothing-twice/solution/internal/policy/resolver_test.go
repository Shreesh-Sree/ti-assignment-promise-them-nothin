package policy_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"relayapi/internal/policy"
	"relayapi/internal/ratelimit"
)

const testConfigYAML = `
tiers:
  starter:
    rpm: 60
  growth:
    rpm: 300
  enterprise:
    rpm: 0

customers:
  - id: cust_northwind_logistics
    tier: enterprise
    limit_rpm: 300

overrides:
  - customer: cust_northwind_logistics
    limit_rpm: 1200
    window:
      start_utc: "02:00"
      end_utc: "04:00"
      grace_minutes: 60
    expires: "2024-01-05"
    ticket: "OPS-4821"
    reason: "test fixture"
`

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "customers.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func newTestResolver(t *testing.T, contents string, loadClock ratelimit.Clock, logger *slog.Logger) *policy.Resolver {
	t.Helper()
	path := writeConfig(t, contents)
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	r, err := policy.NewResolver(path, loadClock, logger)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

// TestResolveOutsideWindowReturnsContractedLimit: resolver returns 300
// (the contracted limit) when now is nowhere near Northwind's window.
func TestResolveOutsideWindowReturnsContractedLimit(t *testing.T) {
	loadClock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newTestResolver(t, testConfigYAML, loadClock, nil)

	now := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	d := r.Resolve("cust_northwind_logistics", now)

	if d.Limit != 300 {
		t.Errorf("Limit = %d, want 300 (contracted)", d.Limit)
	}
	if d.Reason != "tier_default" {
		t.Errorf("Reason = %q, want %q", d.Reason, "tier_default")
	}
}

// TestResolveInsideWindowReturnsOverrideLimit: resolver returns 1200
// (the override) when now is inside the daily window.
func TestResolveInsideWindowReturnsOverrideLimit(t *testing.T) {
	loadClock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newTestResolver(t, testConfigYAML, loadClock, nil)

	now := time.Date(2024, 1, 2, 2, 30, 0, 0, time.UTC) // inside 02:00-04:00
	d := r.Resolve("cust_northwind_logistics", now)

	if d.Limit != 1200 {
		t.Errorf("Limit = %d, want 1200 (override)", d.Limit)
	}
	if d.Reason != "override_applied" {
		t.Errorf("Reason = %q, want %q", d.Reason, "override_applied")
	}
}

// TestResolveInsideGraceReturnsOverrideLimit folds in the DESIGN-NOTES.md
// Part 1 §3 fix: a batch running past the nominal 04:00 close (documented
// queue-depth-driven late start) must not get cut off mid-job. 04:15 is
// past the nominal end but within this fixture's 60-minute grace.
func TestResolveInsideGraceReturnsOverrideLimit(t *testing.T) {
	loadClock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newTestResolver(t, testConfigYAML, loadClock, nil)

	now := time.Date(2024, 1, 2, 4, 15, 0, 0, time.UTC)
	d := r.Resolve("cust_northwind_logistics", now)

	if d.Limit != 1200 {
		t.Errorf("Limit = %d, want 1200 — still within grace past the nominal window close", d.Limit)
	}
}

// TestResolveAfterGraceReturnsContractedLimit: grace is a bounded pad, not
// an open-ended one — one minute past it, the override is gone.
func TestResolveAfterGraceReturnsContractedLimit(t *testing.T) {
	loadClock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newTestResolver(t, testConfigYAML, loadClock, nil)

	now := time.Date(2024, 1, 2, 5, 1, 0, 0, time.UTC) // one minute past 05:00 (04:00 + 60m grace)
	d := r.Resolve("cust_northwind_logistics", now)

	if d.Limit != 300 {
		t.Errorf("Limit = %d, want 300 — grace period should have run out", d.Limit)
	}
}

// TestResolveAfterExpiryReturnsContractedLimitEvenInsideWindow: resolver
// returns 300 once the expiry date has passed, even when the clock is
// inside the daily window — expiry is checked on every call, not just at
// load time.
func TestResolveAfterExpiryReturnsContractedLimitEvenInsideWindow(t *testing.T) {
	loadClock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newTestResolver(t, testConfigYAML, loadClock, nil)

	// Fixture's override expires 2024-01-05. This is 2024-01-06, 02:30
	// UTC — squarely inside the daily window's time-of-day — but the
	// calendar date is past the expiry.
	now := time.Date(2024, 1, 6, 2, 30, 0, 0, time.UTC)
	d := r.Resolve("cust_northwind_logistics", now)

	if d.Limit != 300 {
		t.Errorf("Limit = %d, want 300 — override should be expired even though the clock is inside its daily window", d.Limit)
	}
	if d.Reason != "tier_default" {
		t.Errorf("Reason = %q, want %q", d.Reason, "tier_default")
	}
}

// TestAuditEventFiresOnlyWhenOverrideApplies: the audit event must not
// fire on ordinary tier-default resolutions, only when an override
// actually changes the effective limit.
func TestAuditEventFiresOnlyWhenOverrideApplies(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	loadClock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r := newTestResolver(t, testConfigYAML, loadClock, logger)

	outside := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	r.Resolve("cust_northwind_logistics", outside)
	if strings.Contains(buf.String(), "override_applied") {
		t.Fatalf("audit log contains override_applied after a request outside the window:\n%s", buf.String())
	}

	inside := time.Date(2024, 1, 2, 2, 30, 0, 0, time.UTC)
	r.Resolve("cust_northwind_logistics", inside)
	out := buf.String()
	if !strings.Contains(out, "override_applied") {
		t.Fatalf("audit log missing override_applied after a request inside the window:\n%s", out)
	}
	for _, want := range []string{
		"customer_id=cust_northwind_logistics",
		"contracted_limit_rpm=300",
		"effective_limit_rpm=1200",
		"override_ticket=OPS-4821",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log missing %q:\n%s", want, out)
		}
	}
}
