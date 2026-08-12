//go:build fixedwindow

package coordinator

import (
	"sync"
	"time"

	"relayapi/internal/ratelimit"
)

// fixedWindowLimiter is a deliberately broken counter that resets on each
// calendar-aligned minute boundary. It exists solely to prove the harness
// catches exactly the failure mode DESIGN-NOTES.md describes: a client
// that bursts at the end of one minute and again at the start of the next
// can admit up to 2x quota across the boundary. Built behind the
// "fixedwindow" build tag so it is never compiled into the real binary.
type fixedWindowLimiter struct {
	mu      sync.Mutex
	windows map[string]*fwState
	quota   int
}

type fwState struct {
	count    int
	windowID int64 // unix-minute the count applies to
}

func newFixedWindowLimiter(quota int) *fixedWindowLimiter {
	return &fixedWindowLimiter{windows: make(map[string]*fwState), quota: quota}
}

func (f *fixedWindowLimiter) allow(customerID string, now time.Time) ratelimit.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()

	wid := now.Unix() / 60
	st, ok := f.windows[customerID]
	if !ok || st.windowID != wid {
		st = &fwState{windowID: wid}
		f.windows[customerID] = st
	}

	if st.count >= f.quota {
		return ratelimit.Decision{
			Allowed:    false,
			Remaining:  0,
			RetryAfter: time.Duration(60-now.Unix()%60) * time.Second,
			Limit:      f.quota,
			Reason:     "rate_exceeded",
		}
	}

	st.count++
	return ratelimit.Decision{
		Allowed:   true,
		Remaining: f.quota - st.count,
		Limit:     f.quota,
		Reason:    "admitted",
	}
}

// FixedWindowStatic is the Static coordinator with GCRA replaced by a
// fixed-window counter — exactly the algorithm the second prior limiter
// was decommissioned for using. Same interface, same per-node share
// arithmetic, different counting. Only compiled under -tags=fixedwindow.
type FixedWindowStatic struct {
	nodeID    string
	nodeCount int

	mu       sync.Mutex
	limiters map[shareKey]*fixedWindowLimiter
}

func NewFixedWindowStatic(nodeID string, nodeCount int) *FixedWindowStatic {
	return &FixedWindowStatic{
		nodeID:    nodeID,
		nodeCount: nodeCount,
		limiters:  make(map[shareKey]*fixedWindowLimiter),
	}
}

func (s *FixedWindowStatic) Allow(customerID string, globalLimit int, now time.Time) ratelimit.Decision {
	key := shareKey{customerID: customerID, globalLimit: globalLimit}
	s.mu.Lock()
	l, ok := s.limiters[key]
	if !ok {
		l = newFixedWindowLimiter(nodeShare(globalLimit, s.nodeCount))
		s.limiters[key] = l
	}
	s.mu.Unlock()
	return l.allow(customerID, now)
}

func (s *FixedWindowStatic) QuotaState() QuotaState {
	return QuotaState{NodeID: s.nodeID, Mode: "fixedwindow", NodeCount: s.nodeCount}
}
