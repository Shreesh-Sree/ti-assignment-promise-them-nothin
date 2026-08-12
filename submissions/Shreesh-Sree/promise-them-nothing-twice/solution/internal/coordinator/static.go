package coordinator

import (
	"sync"
	"time"

	"relayapi/internal/ratelimit"
)

// Static is the naive coordination strategy from DESIGN-NOTES.md Part 2,
// Strategy A: this node's share of a customer's quota is a fixed
// globalLimit/NodeCount, computed once and never adjusted from observed
// traffic. There is no cross-node communication at all, so there is
// nothing to fail when peers are unreachable — behavior under partition is
// identical to behavior with a healthy network, because nodes never talked
// to each other in the first place.
//
// This is also its known weakness, deliberately left unaddressed here so
// session 5's load test can demonstrate it directly rather than take it on
// faith: round-robin distribution is only even on average. A client using
// keep-alive connections, or ordinary short-term clustering in a
// round-robin sequence, can send this node more than its fixed 1/N share
// in a given window even while the customer's total traffic stays under
// their global limit — and this node has no way to know that, or to borrow
// headroom from a sibling that's under its own share at the same moment.
// The result is a false reject of legitimate traffic. Static does not
// pretend otherwise; Peer (peer.go) exists to fix exactly this.
type Static struct {
	nodeID    string
	nodeCount int
	clock     ratelimit.Clock

	mu       sync.Mutex
	limiters map[shareKey]*ratelimit.Limiter
}

// shareKey caches a limiter per (customer, globalLimit) rather than per
// customer alone. A genuine limit change — e.g. Northwind's override
// window opening or closing — is exactly the kind of event this strategy
// has no live-adaptation story for, so it's treated the same way a brand
// new customer is: a fresh limiter, fresh TAT, starting clean at the new
// limit. That's a real, named gap (a limit change loses in-flight burst
// history), not a hidden one — Peer's mutable-share state (share_gcra.go)
// is what actually solves it, by keeping TAT continuous across a change.
type shareKey struct {
	customerID  string
	globalLimit int
}

// NewStatic returns a Static coordinator for this node, splitting every
// customer's limit evenly across nodeCount nodes.
func NewStatic(nodeID string, nodeCount int, clock ratelimit.Clock) *Static {
	if nodeCount < 1 {
		panic("coordinator: NewStatic requires nodeCount >= 1")
	}
	return &Static{
		nodeID:    nodeID,
		nodeCount: nodeCount,
		clock:     clock,
		limiters:  make(map[shareKey]*ratelimit.Limiter),
	}
}

// nodeShare divides globalLimit evenly across nodeCount nodes, rounding up
// so the sum of per-node shares is never less than globalLimit (a customer
// never loses budget to integer division; at most nodeCount-1 extra
// requests of slack get distributed across nodes when globalLimit doesn't
// divide evenly — an intentional, documented direction of rounding, since
// Priya's rule is "never over-limit the total," and rounding shares up
// biases toward that same direction rather than starving a node's share
// below its true fraction).
func nodeShare(globalLimit, nodeCount int) int {
	return (globalLimit + nodeCount - 1) / nodeCount
}

// Allow implements Coordinator.
func (s *Static) Allow(customerID string, globalLimit int, now time.Time) ratelimit.Decision {
	limiter := s.limiterFor(customerID, globalLimit)
	return limiter.AllowAt(customerID, now)
}

func (s *Static) limiterFor(customerID string, globalLimit int) *ratelimit.Limiter {
	key := shareKey{customerID: customerID, globalLimit: globalLimit}

	s.mu.Lock()
	defer s.mu.Unlock()

	if l, ok := s.limiters[key]; ok {
		return l
	}
	share := nodeShare(globalLimit, s.nodeCount)
	l := ratelimit.NewLimiter(s.clock, ratelimit.Params{
		Quota:  share,
		Period: time.Minute,
		Burst:  NodeBurst, // DECISIONS.md's Burst tradeoff, adopted — worst case is now share+NodeBurst per node, not share exactly
	})
	s.limiters[key] = l
	return l
}

// QuotaState implements Coordinator.
func (s *Static) QuotaState() QuotaState {
	s.mu.Lock()
	defer s.mu.Unlock()

	shares := make([]CustomerShare, 0, len(s.limiters))
	for key := range s.limiters {
		shares = append(shares, CustomerShare{
			CustomerID:  key.customerID,
			GlobalLimit: key.globalLimit,
			NodeShare:   nodeShare(key.globalLimit, s.nodeCount),
			LastUpdated: time.Time{}, // static shares never change after first computation — there is no "last rebalanced"
		})
	}
	return QuotaState{
		NodeID:      s.nodeID,
		Mode:        "static",
		NodeCount:   s.nodeCount,
		IsProposer:  false,
		RoundNumber: 0,
		Shares:      shares,
	}
}
