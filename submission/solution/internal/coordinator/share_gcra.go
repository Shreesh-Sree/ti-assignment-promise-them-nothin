package coordinator

import (
	"sync"
	"time"

	"relayapi/internal/ratelimit"
)

// shareState is one customer's local GCRA enforcement state on this node,
// with a live-mutable Quota.
//
// ratelimit.Limiter (this session does not own or modify that package)
// fixes its Params at construction with no update path, by design — it
// was built for a single fixed limit per instance. The peer coordinator
// needs the opposite: a customer's node-local share changes as the
// background rebalancer runs, and DESIGN-NOTES.md's corrected invariant
// depends specifically on TAT carrying forward unchanged across that
// change — resetting it on every rebalance would transiently re-open the
// exact over-admission window a fresh limiter's zero-value TAT allows.
//
// So this reproduces ratelimit's decide() formula exactly (same emission-
// interval spacing, same TAT semantics), parameterized by a Quota field
// that setQuota can update in place without touching tat. It is
// intentionally small and directly testable against the same properties
// ratelimit's own tests check, so the two implementations can be verified
// to agree rather than trusted to by inspection alone.
type shareState struct {
	mu    sync.Mutex
	tat   time.Time
	quota int // current node share (RPM); mutated live by rebalances
	burst int // DECISIONS.md's Burst tradeoff, adopted — see NodeBurst in coordinator.go
}

func newShareState(initialQuota int) *shareState {
	return &shareState{quota: initialQuota, burst: NodeBurst}
}

// setQuota changes this customer's node-local share. tat is untouched —
// that's the entire point. A shrink makes future admissions stricter
// immediately (a smaller quota lengthens the emission interval used on
// the very next decision); a grow loosens them — either way, nothing
// already decided is revisited.
func (s *shareState) setQuota(quota int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quota = quota
}

func (s *shareState) currentQuota() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quota
}

// allow runs one GCRA admission check against the customer's current
// quota (read under the same lock as the TAT it's checked against, so a
// concurrent setQuota can never be applied to one half of a decision and
// not the other) and period, at the given arrival time.
func (s *shareState) allow(now time.Time, period time.Duration) ratelimit.Decision {
	s.mu.Lock()
	defer s.mu.Unlock()

	quota := s.quota
	if quota < 1 {
		quota = 1 // a customer with a momentarily-zero share (mid-shrink, nothing reassigned yet) still gets the floor rather than a divide-by-zero
	}
	emission := period / time.Duration(quota)
	burstOffset := time.Duration(s.burst) * emission

	allowAt := s.tat.Add(-burstOffset)
	if now.Before(allowAt) {
		return ratelimit.Decision{
			Allowed:    false,
			Remaining:  0,
			RetryAfter: allowAt.Sub(now),
			Limit:      quota,
			Reason:     "rate_exceeded",
		}
	}

	newTAT := s.tat
	if now.After(newTAT) {
		newTAT = now
	}
	newTAT = newTAT.Add(emission)
	s.tat = newTAT

	margin := newTAT.Sub(now)
	remaining := 0
	if margin <= burstOffset {
		remaining = int((burstOffset-margin)/emission) + 1
	}

	return ratelimit.Decision{
		Allowed:    true,
		Remaining:  remaining,
		RetryAfter: 0,
		Limit:      quota,
		Reason:     "admitted",
	}
}
