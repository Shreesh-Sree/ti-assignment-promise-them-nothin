package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"relayapi/internal/ratelimit"
)

// PeerConfig configures the peer coordination strategy from
// DESIGN-NOTES.md Part 2 and "Who proposes a round": static per-node
// shares, rebalanced periodically by a single, statically-designated
// proposer, using the two-phase shrink-before-grow protocol proven safe
// in "Stress-testing the invariant."
type PeerConfig struct {
	NodeID    string
	NodeCount int
	Peers     map[string]string // node id -> base URL, including this node's own entry
	Proposer  string            // node id of the statically-designated proposer; not elected, not failed over

	Clock  ratelimit.Clock
	Logger *slog.Logger

	Period       time.Duration // GCRA period, e.g. time.Minute. Zero means time.Minute.
	PollInterval time.Duration // T_poll: how often the proposer evaluates load and proposes a new split. Zero means 1s.
	AckTimeout   time.Duration // T_ack: per-request timeout for a shrink/grow round-trip to a peer. Zero means 400ms.

	HTTPClient *http.Client // Zero means a client built from AckTimeout.
}

// hysteresisRPM is the minimum per-node share delta that triggers a
// rebalance round for a customer. Below this, the proposer leaves shares
// alone rather than start a round (with its shrink-then-grow round trips,
// each of which involves a real shrink — a real, if small, tightening of
// that node's GCRA pacing) to correct a difference too small to matter.
//
// This was originally 3, on the assumption that EMA smoothing (see
// emaDemand) would be enough to keep the signal quiet at rest. It wasn't:
// even smoothed, per-node demand for one customer at this traffic scale
// (a few requests/node/second) never fully stops random-walking, so a
// low threshold kept triggering small, constant rebalances even when the
// true underlying split was already even — each one a real, if brief,
// tightening of some node's pacing, which is itself a source of false
// rejects, just a smaller-amplitude version of the oscillation problem
// documented on emaDemand. 15 (15% of a 100 RPM baseline share) is sized
// to comfortably exceed that steady-state noise floor for this
// prototype's traffic scale, so a round only fires for a difference large
// enough to plausibly be a real, sustained shift rather than sampling
// noise. It is a real, named tuning knob, not a proof — a customer at
// much higher absolute RPM would need this reconsidered as a fraction of
// share rather than a fixed count.
const hysteresisRPM = 15

// Peer implements Coordinator using the two-phase rebalancing protocol.
// Every node runs the same binary and the same Peer type; only the
// statically-configured Proposer field makes one of them actually start
// the background rebalance loop (see Run) — the others only ever receive
// and apply shrink/grow instructions, and answer status polls.
type Peer struct {
	cfg  PeerConfig
	http *http.Client

	mu        sync.Mutex
	customers map[string]*customerState

	proposerMu sync.Mutex // serializes the proposer's own round-numbering; irrelevant on non-proposer nodes
	peerHealth map[string]PeerHealth

	// emaDemand holds the proposer's smoothed view of each customer's
	// per-node demand: customerID -> nodeID -> exponential moving average
	// of requests/tick. Proposer-only state, touched from a single
	// goroutine (runRebalanceTick), so it needs no lock of its own.
	//
	// Why this exists: a raw single-tick demand count is a tiny, noisy
	// sample at this traffic scale (PollInterval=1s against ~1-2
	// requests/node/tick for a 300 RPM customer split three ways).
	// Feeding that noise directly into proportionalSplit was tried first
	// and made things worse than Static, not better — targets swung
	// between e.g. [60,120,120] and [180,60,60] every single tick, and
	// each swing forced a real shrink (a real reduction in GCRA's
	// emission rate) that produced its own false rejects, on top of
	// whatever Static was already causing. Smoothing the signal the
	// proposer acts on is what makes "track real demand" different from
	// "chase noise" — the load test in DESIGN-NOTES.md-adjacent session
	// notes names this as the concrete failure mode found, not a
	// hypothetical one.
	emaDemand map[string]map[string]float64
}

// demandEMAAlpha weights how much a single tick's raw demand count moves
// the smoothed estimate. Low alpha means slow-moving, noise-resistant,
// slower to react to a genuine shift; high alpha means the opposite. 0.2
// gives an effective averaging window of several seconds at a 1s
// PollInterval — enough to average out the single-digit sample noise this
// prototype's traffic scale produces, while still adapting well within
// Northwind's 90-120 minute batch window if this were driving that case.
const demandEMAAlpha = 0.2

type customerState struct {
	share       *shareState
	demand      atomic.Int64 // requests observed since this node's status was last polled; reset on read
	globalLimit int          // last known effective limit, learned from this node's own Allow() calls
	lastApplied uint64       // highest round number this node has applied for this customer (fences stale messages)
	lastUpdated time.Time
}

// NewPeer validates cfg and returns a Peer ready to enforce admission
// decisions immediately (using the same static bootstrap split Strategy A
// uses, per customer, until the first rebalance round adjusts it) — Run
// is what starts the background rebalancing; a Peer is safe to use for
// Allow before Run is called, it just won't adapt yet.
func NewPeer(cfg PeerConfig) (*Peer, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("coordinator: PeerConfig.NodeID is required")
	}
	if cfg.NodeCount < 1 {
		return nil, fmt.Errorf("coordinator: PeerConfig.NodeCount must be >= 1")
	}
	if _, ok := cfg.Peers[cfg.NodeID]; !ok {
		return nil, fmt.Errorf("coordinator: PeerConfig.Peers must include this node's own id %q", cfg.NodeID)
	}
	if cfg.Proposer == "" {
		return nil, fmt.Errorf("coordinator: PeerConfig.Proposer is required")
	}
	if _, ok := cfg.Peers[cfg.Proposer]; !ok {
		return nil, fmt.Errorf("coordinator: PeerConfig.Proposer %q is not in Peers", cfg.Proposer)
	}
	if cfg.Period == 0 {
		cfg.Period = time.Minute
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.AckTimeout == 0 {
		cfg.AckTimeout = 400 * time.Millisecond
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.AckTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Peer{
		cfg:        cfg,
		http:       cfg.HTTPClient,
		customers:  make(map[string]*customerState),
		peerHealth: make(map[string]PeerHealth),
		emaDemand:  make(map[string]map[string]float64),
	}, nil
}

// isProposer reports whether this node is the one statically designated
// to run the rebalance loop. Not computed from liveness or an election —
// a literal config comparison, per the "no automatic takeover" decision.
func (p *Peer) isProposer() bool { return p.cfg.NodeID == p.cfg.Proposer }

// Run starts the background rebalance loop if this node is the proposer,
// and does nothing otherwise (a non-proposer node only ever responds to
// incoming HTTP calls from the real proposer). Safe to call on every
// node uniformly — the proposer check is internal.
func (p *Peer) Run(ctx context.Context) {
	if !p.isProposer() {
		return
	}
	go func() {
		ticker := time.NewTicker(p.cfg.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.runRebalanceTick(ctx)
			}
		}
	}()
}

// Allow implements Coordinator.
func (p *Peer) Allow(customerID string, globalLimit int, now time.Time) ratelimit.Decision {
	cs := p.customerFor(customerID, globalLimit)
	cs.demand.Add(1)
	return cs.share.allow(now, p.cfg.Period)
}

// customerFor returns this customer's local state, creating it — bootstrapped
// to the same static globalLimit/NodeCount split Strategy A uses — the
// first time this node sees them, on whichever code path (a client
// request, or an incoming shrink/grow from the proposer) sees them first.
// globalLimit of 0 (from the apply-share path, which doesn't know it) is
// only ever used for a brand-new entry's bootstrap share, and only until
// something better is known.
func (p *Peer) customerFor(customerID string, globalLimit int) *customerState {
	p.mu.Lock()
	defer p.mu.Unlock()

	cs, ok := p.customers[customerID]
	if !ok {
		initial := 1
		if globalLimit > 0 {
			initial = nodeShare(globalLimit, p.cfg.NodeCount)
		}
		cs = &customerState{share: newShareState(initial), globalLimit: globalLimit}
		p.customers[customerID] = cs
		return cs
	}
	if globalLimit > 0 {
		cs.globalLimit = globalLimit
	}
	return cs
}

// QuotaState implements Coordinator.
func (p *Peer) QuotaState() QuotaState {
	p.mu.Lock()
	shares := make([]CustomerShare, 0, len(p.customers))
	var maxRound uint64
	for id, cs := range p.customers {
		shares = append(shares, CustomerShare{
			CustomerID:  id,
			GlobalLimit: cs.globalLimit,
			NodeShare:   cs.share.currentQuota(),
			LastUpdated: cs.lastUpdated,
		})
		if cs.lastApplied > maxRound {
			maxRound = cs.lastApplied
		}
	}
	sort.Slice(shares, func(i, j int) bool { return shares[i].CustomerID < shares[j].CustomerID })

	peers := make([]PeerHealth, 0, len(p.peerHealth))
	for _, h := range p.peerHealth {
		peers = append(peers, h)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].NodeID < peers[j].NodeID })
	p.mu.Unlock()

	return QuotaState{
		NodeID:      p.cfg.NodeID,
		Mode:        "peer",
		NodeCount:   p.cfg.NodeCount,
		Proposer:    p.cfg.Proposer,
		IsProposer:  p.isProposer(),
		RoundNumber: maxRound,
		Shares:      shares,
		Peers:       peers,
	}
}

// RegisterRoutes adds the peer-to-peer endpoints used only by the
// background rebalancer — never by client-facing demo traffic — to mux.
// httpapi.Server calls this via a type assertion so these routes live on
// the same listener as the public API, without httpapi needing to know
// anything about the rebalance protocol itself.
func (p *Peer) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /internal/coordinator/status", p.handleStatus)
	mux.HandleFunc("POST /internal/coordinator/apply-share", p.handleApplyShare)
}

type statusResponse struct {
	NodeID    string                    `json:"node_id"`
	Customers map[string]customerStatus `json:"customers"`
}

type customerStatus struct {
	Share       int   `json:"share"`
	GlobalLimit int   `json:"global_limit"`
	Demand      int64 `json:"demand_since_last_poll"`
}

// handleStatus answers "what is your current share and recent demand for
// every customer you know about" and resets each customer's demand
// counter to zero on read — a pull-based heartbeat, polled only by the
// proposer, only from its background goroutine.
func (p *Peer) handleStatus(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	out := statusResponse{NodeID: p.cfg.NodeID, Customers: make(map[string]customerStatus, len(p.customers))}
	for id, cs := range p.customers {
		out.Customers[id] = customerStatus{
			Share:       cs.share.currentQuota(),
			GlobalLimit: cs.globalLimit,
			Demand:      cs.demand.Swap(0),
		}
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

type applyShareRequest struct {
	CustomerID string `json:"customer_id"`
	Round      uint64 `json:"round"`
	Quota      int    `json:"quota"`
}

type applyShareResponse struct {
	Applied bool   `json:"applied"`
	Reason  string `json:"reason,omitempty"`
}

// handleApplyShare is the single endpoint both shrink and grow
// instructions use — this node's behavior on receipt is identical either
// direction (apply the new quota immediately, TAT untouched). The safety
// property (sum of shares never exceeds the global quota) comes entirely
// from the PROPOSER's discipline in sending shrinks before grows and
// gating grows on shrink acknowledgment (runRebalanceTick) — not from
// anything this handler does. What this handler does own is the other
// half of the safety proof: fencing stale, out-of-order messages via a
// strictly-increasing round number per customer, so a delayed message
// from an abandoned round can never be misapplied after a newer one has
// already landed.
func (p *Peer) handleApplyShare(w http.ResponseWriter, r *http.Request) {
	var req applyShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	cs := p.customerFor(req.CustomerID, 0)

	p.mu.Lock()
	if req.Round <= cs.lastApplied {
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(applyShareResponse{Applied: false, Reason: "stale_round"})
		return
	}
	cs.lastApplied = req.Round
	cs.lastUpdated = p.cfg.Clock.Now()
	p.mu.Unlock()

	cs.share.setQuota(req.Quota)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(applyShareResponse{Applied: true})
}

// runRebalanceTick is one pass of the proposer's loop: poll every node's
// current share and recent demand, compute a demand-proportional target
// split per customer, and — for any customer whose target differs enough
// from its current split to be worth it — run one full shrink-before-grow
// round. Only ever called on the proposer node, only from Run's goroutine,
// so there is exactly one of these in flight at a time by construction —
// the "at most one round" rule from DESIGN-NOTES.md is satisfied by there
// being a single caller, not by an explicit lock.
func (p *Peer) runRebalanceTick(ctx context.Context) {
	statuses := p.pollAllNodes(ctx)

	// Union of every customer any node currently knows about.
	seen := map[string]bool{}
	for _, st := range statuses {
		for id := range st.Customers {
			seen[id] = true
		}
	}

	for customerID := range seen {
		p.rebalanceCustomer(ctx, customerID, statuses)
	}
}

// pollAllNodes fetches /internal/coordinator/status from every peer
// (using this node's own in-memory state directly for itself, to avoid
// an unnecessary network hop) and updates peerHealth from the result.
// A peer that fails to answer within AckTimeout is recorded unreachable
// and simply excluded from this tick's rebalancing — Strategy B's
// documented degrade-to-Strategy-A behavior for that node, until it
// answers again.
func (p *Peer) pollAllNodes(ctx context.Context) map[string]statusResponse {
	out := make(map[string]statusResponse, len(p.cfg.Peers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for nodeID, baseURL := range p.cfg.Peers {
		if nodeID == p.cfg.NodeID {
			mu.Lock()
			out[nodeID] = p.localStatus()
			p.recordHealth(nodeID, true)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(nodeID, baseURL string) {
			defer wg.Done()
			st, err := p.fetchStatus(ctx, baseURL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				p.recordHealth(nodeID, false)
				return
			}
			out[nodeID] = st
			p.recordHealth(nodeID, true)
		}(nodeID, baseURL)
	}
	wg.Wait()
	return out
}

func (p *Peer) localStatus() statusResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := statusResponse{NodeID: p.cfg.NodeID, Customers: make(map[string]customerStatus, len(p.customers))}
	for id, cs := range p.customers {
		out.Customers[id] = customerStatus{
			Share:       cs.share.currentQuota(),
			GlobalLimit: cs.globalLimit,
			Demand:      cs.demand.Swap(0),
		}
	}
	return out
}

func (p *Peer) fetchStatus(ctx context.Context, baseURL string) (statusResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.AckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/internal/coordinator/status", nil)
	if err != nil {
		return statusResponse{}, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return statusResponse{}, err
	}
	defer resp.Body.Close()
	var st statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return statusResponse{}, err
	}
	return st, nil
}

func (p *Peer) recordHealth(nodeID string, reachable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.peerHealth[nodeID]
	h.NodeID = nodeID
	h.Reachable = reachable
	if reachable {
		h.LastSeen = p.cfg.Clock.Now()
	}
	p.peerHealth[nodeID] = h
}

// smoothDemand folds one tick's raw demand count into this (customer,
// node) pair's exponential moving average and returns the updated value.
// Proposer-only, single-goroutine — see the emaDemand field comment.
func (p *Peer) smoothDemand(customerID, nodeID string, raw float64) float64 {
	byNode, ok := p.emaDemand[customerID]
	if !ok {
		byNode = make(map[string]float64)
		p.emaDemand[customerID] = byNode
	}
	prev, seen := byNode[nodeID]
	if !seen {
		// Anchor the first observation to the raw value itself — no prior
		// estimate to blend with, and starting at 0 would bias the very
		// first round toward starving every node until the EMA catches up.
		byNode[nodeID] = raw
		return raw
	}
	next := demandEMAAlpha*raw + (1-demandEMAAlpha)*prev
	byNode[nodeID] = next
	return next
}

// rebalanceCustomer computes and, if warranted, applies one customer's
// new target split.
func (p *Peer) rebalanceCustomer(ctx context.Context, customerID string, statuses map[string]statusResponse) {
	globalLimit := p.knownGlobalLimit(customerID, statuses)
	if globalLimit <= 0 {
		// This node (the proposer) has never itself resolved this
		// customer's policy limit, so it has no authoritative total to
		// split. Skip this tick; round-robin traffic means the proposer
		// will see this customer directly within a few ticks.
		return
	}

	var nodes []nodeSplit
	for nodeID := range p.cfg.Peers {
		st, ok := statuses[nodeID]
		if !ok {
			continue // unreachable this tick; excluded from the split entirely, its last-applied share stands
		}
		cs, ok := st.Customers[customerID]
		current := cs.Share
		if !ok {
			current = nodeShare(globalLimit, p.cfg.NodeCount) // hasn't seen this customer yet; assume the static bootstrap it would have used
		}
		nodes = append(nodes, nodeSplit{nodeID: nodeID, current: current, demand: cs.Demand})
	}
	if len(nodes) == 0 {
		return
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].nodeID < nodes[j].nodeID }) // deterministic order for the remainder-distribution tie-break

	weights := make([]int64, len(nodes))
	for i, n := range nodes {
		smoothed := p.smoothDemand(customerID, n.nodeID, float64(n.demand))
		// Scale to an integer weight at fixed precision; proportionalSplit
		// only cares about relative magnitude, so the scale factor is
		// arbitrary as long as it's shared and large enough to preserve
		// the EMA's fractional resolution.
		weights[i] = int64(smoothed*1000) + 1 // +1: a node that has been observed but is currently at zero smoothed demand still gets a nonzero weight, so it isn't starved to a hard 0 share by one quiet tick
	}
	targets := proportionalSplit(globalLimit, weights)

	deltas := make(map[string]int, len(nodes))
	maxAbsDelta := 0
	for i, n := range nodes {
		d := targets[i] - n.current
		deltas[n.nodeID] = d
		if abs(d) > maxAbsDelta {
			maxAbsDelta = abs(d)
		}
	}
	if maxAbsDelta < hysteresisRPM {
		return // not worth a round
	}

	p.proposerMu.Lock()
	defer p.proposerMu.Unlock()

	cs := p.customerFor(customerID, globalLimit)
	p.mu.Lock()
	round := cs.lastApplied + 1
	p.mu.Unlock()

	// Phase 1: shrink. Every node whose target is below its current share
	// must apply and acknowledge before any grow is sent.
	shrinkOK := true
	for _, n := range nodes {
		d := deltas[n.nodeID]
		if d >= 0 {
			continue
		}
		if !p.applyShare(ctx, n.nodeID, customerID, round, targets[indexOf(nodes, n.nodeID)]) {
			shrinkOK = false
		}
	}
	if !shrinkOK {
		p.cfg.Logger.Warn("rebalance_round_abandoned", "customer_id", customerID, "round", round, "reason", "shrink_not_acknowledged")
		return // round stalls here; every reachable state so far still sums to <= globalLimit, per the proof
	}

	// Phase 2: grow. Only reached once every shrink above is confirmed.
	for _, n := range nodes {
		d := deltas[n.nodeID]
		if d <= 0 {
			continue
		}
		if !p.applyShare(ctx, n.nodeID, customerID, round, targets[indexOf(nodes, n.nodeID)]) {
			p.cfg.Logger.Warn("rebalance_grow_not_acknowledged", "customer_id", customerID, "round", round, "node_id", n.nodeID)
			// Not a safety problem (nothing was over-granted — this node
			// just didn't get bigger), only a liveness one: it's picked
			// up again next tick since targets are recomputed from
			// observed reality, not from this round's intent.
		}
	}

	p.cfg.Logger.Info("rebalance_round_applied", "customer_id", customerID, "round", round, "targets", fmt.Sprintf("%v", targets), "global_limit", globalLimit)
}

// knownGlobalLimit prefers this node's own cached value (the only one it
// can act on with authority — see rebalanceCustomer's comment); falling
// back to any peer's reported value is deliberately not done, so a
// disagreement about the limit itself never gets silently resolved by
// majority vote.
func (p *Peer) knownGlobalLimit(customerID string, statuses map[string]statusResponse) int {
	if st, ok := statuses[p.cfg.NodeID]; ok {
		if cs, ok := st.Customers[customerID]; ok {
			return cs.GlobalLimit
		}
	}
	return 0
}

// applyShare sends one shrink or grow instruction and reports whether it
// was applied. Applying to this node itself never goes over HTTP.
func (p *Peer) applyShare(ctx context.Context, nodeID, customerID string, round uint64, quota int) bool {
	if nodeID == p.cfg.NodeID {
		cs := p.customerFor(customerID, 0)
		p.mu.Lock()
		if round <= cs.lastApplied {
			p.mu.Unlock()
			return false
		}
		cs.lastApplied = round
		cs.lastUpdated = p.cfg.Clock.Now()
		p.mu.Unlock()
		cs.share.setQuota(quota)
		return true
	}

	baseURL := p.cfg.Peers[nodeID]
	body, _ := json.Marshal(applyShareRequest{CustomerID: customerID, Round: round, Quota: quota})
	reqCtx, cancel := context.WithTimeout(ctx, p.cfg.AckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+"/internal/coordinator/apply-share", bytes.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		p.recordHealth(nodeID, false)
		return false
	}
	defer resp.Body.Close()
	p.recordHealth(nodeID, true)

	var out applyShareResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.Applied
}

// proportionalSplit divides total across weights proportionally, using
// the largest-remainder method so the result always sums to exactly
// total (never less, never more — the property the corrected invariant
// depends on) regardless of rounding. All-zero weights fall back to an
// even split.
func proportionalSplit(total int, weights []int64) []int {
	n := len(weights)
	result := make([]int, n)
	if n == 0 {
		return result
	}

	var sum int64
	for _, w := range weights {
		sum += w
	}
	if sum == 0 {
		return proportionalSplit(total, evenWeights(n))
	}

	type remainder struct {
		idx int
		rem float64
	}
	var rems []remainder
	allocated := 0
	for i, w := range weights {
		exact := float64(total) * float64(w) / float64(sum)
		floor := int(exact)
		result[i] = floor
		allocated += floor
		rems = append(rems, remainder{idx: i, rem: exact - float64(floor)})
	}

	sort.Slice(rems, func(i, j int) bool { return rems[i].rem > rems[j].rem })
	leftover := total - allocated
	for i := 0; i < leftover && i < len(rems); i++ {
		result[rems[i].idx]++
	}
	return result
}

func evenWeights(n int) []int64 {
	w := make([]int64, n)
	for i := range w {
		w[i] = 1
	}
	return w
}

type nodeSplit struct {
	nodeID  string
	current int
	demand  int64
}

func indexOf(nodes []nodeSplit, nodeID string) int {
	for i, n := range nodes {
		if n.nodeID == nodeID {
			return i
		}
	}
	return -1
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
