package coordinator_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"relayapi/internal/coordinator"
	"relayapi/internal/ratelimit"
)

// TestProportionalSplitSumsExactly checks the arithmetic primitive the
// rebalancer depends on directly: whatever the weights, the split always
// sums to exactly total — the property the corrected invariant in
// DESIGN-NOTES.md needs to hold at every rest state.
func TestProportionalSplitSumsExactly(t *testing.T) {
	cases := []struct {
		total   int
		weights []int64
	}{
		{300, []int64{100, 100, 100}},
		{300, []int64{1, 1, 1}},
		{300, []int64{0, 0, 0}}, // falls back to even split
		{100, []int64{7, 3, 5}}, // doesn't divide evenly
		{1, []int64{1, 1, 1}},   // total smaller than node count
	}
	for _, c := range cases {
		got := exportedProportionalSplit(c.total, c.weights)
		sum := 0
		for _, v := range got {
			sum += v
		}
		if sum != c.total {
			t.Errorf("proportionalSplit(%d, %v) = %v, sum = %d, want %d", c.total, c.weights, got, sum, c.total)
		}
		for _, v := range got {
			if v < 0 {
				t.Errorf("proportionalSplit(%d, %v) = %v has a negative share", c.total, c.weights, got)
			}
		}
	}
}

// peerHarness wires three in-process Peer coordinators together over real
// HTTP (httptest servers), node-1 as the statically-designated proposer,
// so the rebalance protocol runs exactly as it would across real nodes —
// just without docker-compose and nginx in the loop.
type peerHarness struct {
	nodes []*coordinator.Peer
	urls  []string
	clock *ratelimit.FakeClock
}

func newPeerHarness(t *testing.T, pollInterval time.Duration) *peerHarness {
	t.Helper()
	clock := ratelimit.NewFakeClock(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Reserve three addresses first, so every node's PeerConfig.Peers map
	// is complete before any of them start serving.
	var servers []*httptest.Server
	var mux []*http.ServeMux
	for range 3 {
		m := http.NewServeMux()
		s := httptest.NewServer(m)
		servers = append(servers, s)
		mux = append(mux, m)
	}

	peersMap := map[string]string{
		"node-1": servers[0].URL,
		"node-2": servers[1].URL,
		"node-3": servers[2].URL,
	}

	h := &peerHarness{clock: clock}
	for i := 0; i < 3; i++ {
		nodeID := []string{"node-1", "node-2", "node-3"}[i]
		pc, err := coordinator.NewPeer(coordinator.PeerConfig{
			NodeID:       nodeID,
			NodeCount:    3,
			Peers:        peersMap,
			Proposer:     "node-1",
			Clock:        clock,
			Logger:       logger,
			PollInterval: pollInterval,
			AckTimeout:   200 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("NewPeer(%s): %v", nodeID, err)
		}
		pc.RegisterRoutes(mux[i])
		h.nodes = append(h.nodes, pc)
		h.urls = append(h.urls, servers[i].URL)
	}

	t.Cleanup(func() {
		for _, s := range servers {
			s.Close()
		}
	})
	return h
}

func exportedProportionalSplit(total int, weights []int64) []int {
	// proportionalSplit is unexported; this test lives in package
	// coordinator_test (black-box), so it goes through a tiny same-package
	// shim instead of reaching into internals. See split_shim_test.go.
	return coordinator.ExportedProportionalSplitForTest(total, weights)
}

// TestPeerRebalanceConvergesTowardDemand drives skewed demand — node-1
// gets most requests, node-2 and node-3 get few — directly against each
// node's Allow (bypassing real HTTP client traffic, since this test is
// about the rebalance protocol converging, not about round robin), lets
// the proposer's background loop run against a real clock briefly, and
// checks shares moved toward the observed demand shape rather than
// staying frozen at the static 100/100/100 split.
func TestPeerRebalanceConvergesTowardDemand(t *testing.T) {
	h := newPeerHarness(t, 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range h.nodes {
		n.Run(ctx)
	}

	const globalLimit = 300
	now := h.clock.Now()

	// Skew demand heavily toward node-1: 100 requests on node-1, 10 each
	// on node-2 and node-3, well beyond hysteresisRPM's threshold.
	for range 100 {
		h.nodes[0].Allow("cust", globalLimit, now)
	}
	for range 10 {
		h.nodes[1].Allow("cust", globalLimit, now)
		h.nodes[2].Allow("cust", globalLimit, now)
	}

	// Let the real-time background loop poll and rebalance. PollInterval
	// is 50ms; give it several cycles.
	deadline := time.After(2 * time.Second)
	converged := false
	for !converged {
		select {
		case <-deadline:
			t.Fatalf("rebalance did not converge within 2s; node-1 share = %d", h.nodes[0].QuotaState().Shares[0].NodeShare)
		case <-time.After(20 * time.Millisecond):
			share1 := shareFor(h.nodes[0].QuotaState(), "cust")
			if share1 > 130 { // meaningfully above the static 100 baseline
				converged = true
			}
		}
	}
}

func shareFor(qs coordinator.QuotaState, customerID string) int {
	for _, s := range qs.Shares {
		if s.CustomerID == customerID {
			return s.NodeShare
		}
	}
	return -1
}
