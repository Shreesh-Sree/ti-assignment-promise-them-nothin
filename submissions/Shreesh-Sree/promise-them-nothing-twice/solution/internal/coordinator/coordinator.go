// Package coordinator answers one question per request: does *this* node
// admit it, given the customer's globally-configured limit (from
// internal/policy) and however many other stateless nodes might also be
// enforcing that same limit right now.
//
// It implements the two strategies worked out in DESIGN-NOTES.md Part 2:
//
//   - Static: each node gets a fixed share (globalLimit / node count),
//     never adjusted from live traffic. Simple, provably safe, but a burst
//     landing unevenly across nodes can false-reject legitimate traffic
//     under the global limit — this is the naive baseline session 5's
//     load test is expected to demonstrate the failure mode of.
//   - Peer: a single statically-designated proposer periodically
//     rebalances shares across nodes using a two-phase shrink-before-grow
//     protocol (shrinks confirmed before any grow is sent), so shares
//     track actual per-node demand instead of a fixed 1/N split, while
//     the sum of shares in flight never exceeds the global limit at any
//     instant — the corrected invariant proven in DESIGN-NOTES.md.
//
// Both are exercised behind the same Coordinator interface so httpapi
// never needs to know which one it's talking to.
package coordinator

import (
	"time"

	"relayapi/internal/ratelimit"
)

// NodeBurst is the Burst tradeoff from DESIGN-NOTES.md Part 3 and
// DECISIONS.md, adopted: τ=1 tolerance per node, applied identically by
// both coordination strategies (Static's ratelimit.Params.Burst and
// Peer's shareState.burst), so the provable worst case across any rolling
// 60-second window becomes quota+3 (three nodes at τ=1), not quota
// exactly and not an open-ended loosening either.
//
// This is one package constant, not a per-customer or per-tier value.
// Nothing in internal/policy resolves a burst tolerance today — only
// Limit — so a starter-tier customer at 60 RPM and Northwind at 1200 RPM
// currently get the same absolute τ=1, even though they might reasonably
// warrant different absolute tolerances. Making it vary would mean
// threading a second value through policy.Decision and Coordinator.Allow
// the same way globalLimit already is — a straightforward extension of
// the existing pattern, not a redesign, but real work not done here.
const NodeBurst = 1

// Coordinator decides whether this node admits a request for customerID,
// given that customer's current global limit (resolved by internal/policy
// from config, independent of coordination). now is the request's arrival
// time, supplied by the caller — coordinator makes no clock calls of its
// own on the request path, matching the same no-time.Sleep-in-tests,
// inject-the-clock discipline the rest of this codebase uses.
type Coordinator interface {
	Allow(customerID string, globalLimit int, now time.Time) ratelimit.Decision

	// QuotaState reports this node's current view of the world — its own
	// shares, and (for the peer implementation) proposer identity, round
	// number, and peer health — for the /internal/quota-state endpoint.
	// It must be cheap and take no locks that the request path also holds
	// for long, since it can be polled at any time.
	QuotaState() QuotaState
}

// CustomerShare is one customer's current standing on this node: the
// global limit policy resolved for them, and the slice of it this node is
// currently enforcing.
type CustomerShare struct {
	CustomerID  string    `json:"customer_id"`
	GlobalLimit int       `json:"global_limit_rpm"`
	NodeShare   int       `json:"node_share_rpm"`
	LastUpdated time.Time `json:"last_updated"`
}

// PeerHealth is this node's most recent knowledge of one peer's
// reachability. Populated only by the peer coordinator — the static
// coordinator never talks to peers, so it always reports an empty slice.
type PeerHealth struct {
	NodeID    string    `json:"node_id"`
	Reachable bool      `json:"reachable"`
	LastSeen  time.Time `json:"last_seen"`
}

// QuotaState is the full JSON body served at /internal/quota-state — the
// thing that lets a reviewer (or the load harness) prove, from the
// outside, that shares are what the design claims they are, without
// reading the implementation.
type QuotaState struct {
	NodeID      string          `json:"node_id"`
	Mode        string          `json:"mode"` // "static" or "peer"
	NodeCount   int             `json:"node_count"`
	Proposer    string          `json:"proposer,omitempty"`
	IsProposer  bool            `json:"is_proposer"`
	RoundNumber uint64          `json:"round_number"`
	Shares      []CustomerShare `json:"shares"`
	Peers       []PeerHealth    `json:"peers,omitempty"`
}
