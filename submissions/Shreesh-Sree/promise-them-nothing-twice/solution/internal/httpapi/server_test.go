package httpapi_test

import (
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"relayapi/internal/coordinator"
	"relayapi/internal/httpapi"
	"relayapi/internal/policy"
	"relayapi/internal/ratelimit"
)

func testResolver(t *testing.T, clock ratelimit.Clock) *policy.Resolver {
	t.Helper()
	path := writeTestConfig(t, `
tiers:
  growth:
    rpm: 300
customers:
  - id: cust_a
    tier: growth
`)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	r, err := policy.NewResolver(path, clock, logger)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := f.WriteString(contents); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return f.Name()
}

func newTestServer(t *testing.T) (*httpapi.Server, *ratelimit.FakeClock) {
	t.Helper()
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	resolver := testResolver(t, clock)
	coord := coordinator.NewStatic("node-1", 3, clock)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return httpapi.NewServer("node-1", resolver, coord, clock, logger), clock
}

// TestPingMissingCustomerIDRejected checks the 400 path: no
// X-Customer-Id at all, never reaches the limiter.
func TestPingMissingCustomerIDRejected(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/ping", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestPingUnknownCustomerRejected checks the fail-closed path: a customer
// with no config entry gets 403, not an implicit unmetered pass.
func TestPingUnknownCustomerRejected(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/ping", nil)
	req.Header.Set(httpapi.CustomerIDHeader, "cust_nobody")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestPingHeadersPresentOnSuccess checks every header the task asked for
// is present and sane on a plain 200.
func TestPingHeadersPresentOnSuccess(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/v1/ping", nil)
	req.Header.Set(httpapi.CustomerIDHeader, "cust_a")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Node-Id"); got != "node-1" {
		t.Errorf("X-Node-Id = %q, want %q", got, "node-1")
	}
	if got := rec.Header().Get("X-RateLimit-Limit"); got != "300" {
		t.Errorf("X-RateLimit-Limit = %q, want %q", got, "300")
	}
	if got := rec.Header().Get("X-RateLimit-Remaining"); got == "" {
		t.Errorf("X-RateLimit-Remaining missing")
	}
	if got := rec.Header().Get("X-RateLimit-Reset"); got == "" {
		t.Errorf("X-RateLimit-Reset missing")
	}
}

// TestPingRejectionHasJitteredRetryAfter drains the node's share, spends
// the adopted Burst=1 tolerance (DECISIONS.md's Burst tradeoff — one
// extra request at the same instant as the last admitted one is now
// admitted, not rejected, at this share/quota size; verified against the
// same quota=100 numbers as coordinator's TestStaticSplitsEvenly), then
// checks the request after THAT is a 429 with a Retry-After header
// present and at least 1 second (never 0, per the no-meaningless-zero-
// delay rule).
//
// Before the Burst tradeoff was adopted, the (share+1)th request was the
// one expected to 429 — that assertion depended on the exact Burst=0
// value and broke the moment the coordinator's real enforcement moved to
// Burst=1, exactly as expected for a test asserting the boundary Burst
// controls.
func TestPingRejectionHasJitteredRetryAfter(t *testing.T) {
	s, clock := newTestServer(t)
	const share = 100 // 300 / 3 nodes
	base := clock.Now()
	emission := time.Minute / time.Duration(share)
	for i := range share { // spend the share at exactly the steady rate
		clock.Set(base.Add(time.Duration(i) * emission))
		req := httptest.NewRequest("GET", "/api/v1/ping", nil)
		req.Header.Set(httpapi.CustomerIDHeader, "cust_a")
		rec := httptest.NewRecorder()
		s.Routes().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("setup: expected 200 while draining share, got %d", rec.Code)
		}
	}
	// No further clock advance from here: every remaining request in this
	// test lands at the same instant as the last admitted one.

	// (share+1)th request: the one request of Burst=1 tolerance.
	toleranceReq := httptest.NewRequest("GET", "/api/v1/ping", nil)
	toleranceReq.Header.Set(httpapi.CustomerIDHeader, "cust_a")
	toleranceRec := httptest.NewRecorder()
	s.Routes().ServeHTTP(toleranceRec, toleranceReq)
	if toleranceRec.Code != 200 {
		t.Fatalf("setup: expected 200 for the Burst=1 tolerance request, got %d", toleranceRec.Code)
	}

	// (share+2)th request: tolerance is spent.
	req := httptest.NewRequest("GET", "/api/v1/ping", nil)
	req.Header.Set(httpapi.CustomerIDHeader, "cust_a")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != 429 {
		t.Fatalf("status = %d, want 429 after exhausting node share and Burst=1 tolerance; body=%s", rec.Code, rec.Body.String())
	}
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" || retryAfter == "0" {
		t.Errorf("Retry-After = %q, want a positive value", retryAfter)
	}
}

// TestQuotaStateEndpoint checks /internal/quota-state is servable and
// reports this node's identity.
func TestQuotaStateEndpoint(t *testing.T) {
	s, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/internal/quota-state", nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got == "" {
		t.Errorf("empty body")
	}
}
