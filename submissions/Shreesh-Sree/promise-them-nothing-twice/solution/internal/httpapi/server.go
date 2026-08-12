// Package httpapi is the HTTP surface of RelayAPI: it reads X-Customer-Id,
// asks internal/policy what limit applies, asks internal/coordinator
// whether this node admits the request right now against that limit, and
// translates the answer into an HTTP response — 200 or 429, with the
// headers a reviewer or an enterprise security review would expect to
// find, and nothing else. It owns no rate-limiting logic of its own.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"relayapi/internal/coordinator"
	"relayapi/internal/policy"
	"relayapi/internal/ratelimit"
)

// CustomerIDHeader is the header the API gateway is trusted to have
// already authenticated, per platform-context.md. httpapi trusts it
// verbatim — authenticating the gateway's own identity is out of scope for
// this prototype, same as the rest of the brief treats it.
const CustomerIDHeader = "X-Customer-Id"

// jitterFraction is how much random slack gets added on top of the
// GCRA-computed Retry-After, so a wall of requests rejected at the same
// instant doesn't retry in lockstep and immediately re-collide. 20% is a
// small, defensible number for this prototype — enough to spread a retry
// storm across a meaningful window without making clients wait
// noticeably longer than the real answer.
const jitterFraction = 0.20

// Server is the HTTP handler set. Construct with NewServer; it is safe for
// concurrent use, same as the things it wraps.
type Server struct {
	nodeID   string
	resolver *policy.Resolver
	coord    coordinator.Coordinator
	clock    ratelimit.Clock
	logger   *slog.Logger
}

// NewServer wires a Server from its dependencies. None of them are
// constructed here — main owns startup order and failure handling for
// each (per CLAUDE.md: config that fails to load must stop the process
// before it serves any traffic, which only main can enforce).
func NewServer(nodeID string, resolver *policy.Resolver, coord coordinator.Coordinator, clock ratelimit.Clock, logger *slog.Logger) *Server {
	return &Server{nodeID: nodeID, resolver: resolver, coord: coord, clock: clock, logger: logger}
}

// Routes returns the handler tree: the metered demo resource, and the two
// unmetered introspection endpoints (health, quota state). Never call this
// more than once per Server — http.NewServeMux panics on duplicate
// registration, which is exactly the signal to catch that mistake early.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/ping", s.handlePing)
	mux.HandleFunc("GET /internal/quota-state", s.handleQuotaState)
	mux.HandleFunc("GET /internal/healthz", s.handleHealthz)

	// The peer coordinator needs extra endpoints for its background
	// rebalance protocol (never called by client-facing traffic); the
	// static coordinator needs none. A type assertion keeps httpapi from
	// needing to know which one it's holding.
	if rr, ok := s.coord.(interface{ RegisterRoutes(*http.ServeMux) }); ok {
		rr.RegisterRoutes(mux)
	}
	return mux
}

// handlePing is the thin vertical slice platform-context.md asks for: one
// metered resource, real limiter middleware inline (not a separate
// middleware chain — there's exactly one protected route in this
// prototype, so a chain would be an abstraction with one caller), fake
// customer IDs via the trusted header.
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	// Set on every response, including early rejections — a reviewer
	// proving traffic spreads across all three nodes shouldn't have to
	// filter out the 400/403 responses first.
	w.Header().Set("X-Node-Id", s.nodeID)

	customerID := r.Header.Get(CustomerIDHeader)
	if customerID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_customer_id", "X-Customer-Id header is required")
		return
	}

	now := s.clock.Now()
	policyDecision := s.resolver.Resolve(customerID, now)
	if policyDecision.Reason == "unknown_customer" {
		// Fail closed for a customer we have no config for at all: this is
		// the same under-limiting bias as everything else in this system —
		// an unrecognized customer gets zero budget, not an implicit
		// unmetered pass. See DESIGN-NOTES.md Part 1 on the error direction.
		writeJSONError(w, http.StatusForbidden, "unknown_customer", "customer is not configured")
		return
	}

	decision := s.coord.Allow(customerID, policyDecision.Limit, now)
	s.writeRateLimitHeaders(w, policyDecision.Limit, decision)

	// Logged for every request, admitted or not — this is the raw
	// arrival-timing data an external analysis (inter-arrival gaps at a
	// single node, rolling-window admitted counts across all three) needs
	// to check the system's actual behavior against its proof, rather
	// than trust the proof by inspection alone. now is the same instant
	// the admission decision was made against — not logged separately
	// after the fact — so this is exactly what GCRA saw, not an
	// approximation of it.
	s.logger.Info("request_admission",
		slog.String("node_id", s.nodeID),
		slog.String("customer_id", customerID),
		slog.Time("arrival_time", now),
		slog.Bool("allowed", decision.Allowed),
		slog.Int("node_share_limit", decision.Limit),
	)

	if !decision.Allowed {
		w.Header().Set("Retry-After", jitteredRetryAfterSeconds(decision.RetryAfter))
		writeJSONError(w, http.StatusTooManyRequests, "rate_exceeded", "request rate exceeds the customer's current limit")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"pong":        true,
		"customer_id": customerID,
	})
}

// writeRateLimitHeaders sets X-RateLimit-* on every response, success or
// rejection, per the demo requirement. Limit is the customer's global
// policy limit (contracted or override — a fact every node agrees on
// without coordination, since it comes from config, not live state).
// Remaining and Reset describe *this node's* local enforcement state, not
// a global count — there is deliberately no synchronous cross-node call on
// the request path to produce an exact global remaining, so this is
// documented as node-local rather than presented as more precise than it
// is. Reset is seconds until one more admission would be possible on this
// node: for a continuous GCRA limiter that's the more meaningful notion of
// "reset" than a fixed-window's single reset instant, since GCRA has no
// window boundary to reset at.
func (s *Server) writeRateLimitHeaders(w http.ResponseWriter, globalLimit int, d ratelimit.Decision) {
	w.Header().Set("X-RateLimit-Limit", itoa(globalLimit))
	w.Header().Set("X-RateLimit-Remaining", itoa(d.Remaining))

	var resetSeconds int
	if d.Allowed {
		if d.Limit > 0 {
			resetSeconds = ceilSeconds(time.Minute / time.Duration(d.Limit))
		}
	} else {
		resetSeconds = ceilSeconds(d.RetryAfter)
	}
	w.Header().Set("X-RateLimit-Reset", itoa(resetSeconds))
}

// handleQuotaState serves this node's coordinator.QuotaState as JSON — the
// "make correct vs. incorrect boundary behavior obvious from the harness's
// output alone" requirement, applied to coordination specifically: a
// reviewer (or the load harness) can poll every node's shares and peer
// health directly instead of inferring them from admit/reject counts.
func (s *Server) handleQuotaState(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.coord.QuotaState())
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Node-Id", s.nodeID)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}

// jitteredRetryAfterSeconds adds up to jitterFraction extra random delay
// on top of base, so simultaneously-rejected clients don't all wake up and
// retry at the same instant — which would just recreate the same
// collision one interval later. Never returns less than 1 second: a
// Retry-After of 0 is not a meaningful instruction to a client.
func jitteredRetryAfterSeconds(base time.Duration) string {
	if base <= 0 {
		base = time.Second
	}
	jitter := time.Duration(rand.Float64() * jitterFraction * float64(base))
	return itoa(ceilSeconds(base + jitter))
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := d / time.Second
	if d%time.Second != 0 {
		secs++
	}
	return int(secs)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
