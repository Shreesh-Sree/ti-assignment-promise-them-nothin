# Decisions — Promise Them Nothing Twice

## Conflict resolution

The two memos give directly contradictory orders against the same traffic: Priya requires a hard 429 at the contracted 300 RPM; Marcus requires Northwind never see a 429 while sustaining 800–1200 RPM nightly. Both memos contain an explicit escape hatch — Priya: *"commercial exceptions go through config and audit"*; Marcus: *"a temporary exception mechanism, invisible to the customer."* These converge on a single mechanism: an auditable, config-driven, time-scoped override that raises the effective limit, enforced identically for every customer by the same code path. The enforcement engine has no knowledge that Northwind exists.

**Rejected:** silent code-level bypass (Priya forbids by name); raising everyone's limit (Marcus didn't ask for that); telling Northwind to spread traffic (their ERP can't); queuing excess (81,000-request backlog arithmetic — see `DESIGN-NOTES.md` §"Queuing or buffering"); best-effort soft enforcement (reintroduces prior limiter #1's failure mode); fixed-wall-clock override window without grace (breaks most nights — see Part 1 §3).

**Unresolved gap:** a batch starting late can still outlive the override window. Padded with a grace period sized from the documented worst case; not fully eliminated.

## Technical design

**Algorithm:** GCRA (continuous spacing, one timestamp per customer per node). No fixed-window boundary bug. Provable worst case per rolling 60s window: `(ceil(quota/N) + burst) × N` where N=3 nodes, burst=1 — concretely 105 for a 100 RPM customer (ceil(100/3)=34, (34+1)×3=105), 1254 for Northwind at the current 1250 RPM override ceiling (ceil(1250/3)=417, (417+1)×3=1254).

**Coordination:** static per-node partition of the global limit, with an optional two-phase shrink-before-grow rebalancer behind the same `Coordinator` interface. Safety invariant: `sum(shares) ≤ quota` at every instant — proven in `DESIGN-NOTES.md` Part 2, verified against real captured timestamps in Parts 3–4.

**Redis rejection:** Redis was evaluated and explicitly rejected as the coordination foundation. `platform-context.md` states that Redis may not be available and ops will not provision new infrastructure for a prototype — so a design where every admission decision depends on a Redis round-trip either fails in the documented environment (if Redis is absent) or converts a per-customer correctness property into a whole-service availability property (fail-closed on Redis outage = every customer gets 429'd). This constraint is what drives the static-partition approach with per-node `ceil(quota/N)` shares rather than a central atomic counter, and it is what makes the Burst=1 tradeoff necessary: without a shared counter to borrow from, each node must carry its own local tolerance for the timing jitter it cannot compensate for by redistributing. The full comparison — Redis fail-open vs. fail-closed, blast-radius arithmetic, and why fail-closed is the only option consistent with Priya's error-direction rule but still rejected for this environment — is in `DESIGN-NOTES.md` Part 2.

**Burst tradeoff (adopted):** `coordinator.NodeBurst = 1`. Loosens the exact-quota guarantee by a small, named amount (not open-ended) in exchange for eliminating the false-reject problem that was losing 29–63% of compliant traffic at τ=0. Before/after on the same harness:

| Scenario | Burst=0 | Burst=1 |
|----------|---------|---------|
| northwind-batch reject % | 29.5% | 0% (resolved — see below) |
| window-boundary max/bound | 96/100 | 99/105 |
| over-limit-cutoff max/bound | 47/100 | 105/105 |
| node-failure | PASS (47/100) | PASS (47/105) |
| safety invariant | HOLDS | HOLDS |

## What the harness proves

Five named scenarios (`cmd/harness`), each PASS/FAIL from the output alone. The harness keeps its own client-side timestamps (independent of the server), computes a true rolling 60-second maximum per customer (not calendar-minute bucketing), and cross-checks against server logs without depending on them for the verdict.

A **fixed-window** limiter, tested by swapping in a deliberately broken counter (`make up-fixedwindow`, requires a `-tags fixedwindow` rebuild — the broken code is excluded from a normal `go build` entirely by a build tag, not just guarded by a warning), **fails** the harness when traffic happens to straddle a calendar-minute boundary during `over-limit-cutoff`: the 2x-at-boundary bug lets a node admit up to 2× its per-minute quota in any true 60-second rolling window, producing a max_roll_60s that exceeds the 105 bound. Whether a given run catches this depends on when within the minute the scenario starts; it is a probabilistic demonstration, not a guaranteed single-run failure. The GCRA limiter passes the same scenario in every run, with max_roll_60s landing between 103 and 105 (within the provable bound of 105) across the four runs checked.

**Not yet verified:** the window-boundary override-expiry gap.

**Northwind residual resolved:** the ~3% false-reject rate (18 requests per 600-request run) documented in earlier sessions was eliminated by applying the headroom formula from `DESIGN-NOTES.md` Part 1 to the override ceiling in `configs/customers.yaml`: `P × (1 + T_sync/60) = 1200 × (1 + 2.5/60) = 1250 RPM`. At 1250 RPM the per-node share is `ceil(1250/3) = 417` RPM (emission interval 143.9ms vs 150ms at 1200), which absorbs the nginx multi-worker routing jitter that was causing the residual. Confirmed across two consecutive northwind-batch runs: 600/600 admitted, zero rejects, server cross-check exact match both times. Revised safety bound: `(417 + 1) × 3 = 1254`.

## If I had four more hours

- Key the override to observed batch activity instead of a fixed wall-clock window. Currently the window is wall-clock with a grace period — a batch starting late can still outlive it. A trailing-grace-from-last-request mechanism (or an explicit end-of-batch signal) would close this.
- Make burst tolerance per-tier (thread through `policy.Decision` like `Limit` already is) so enterprise customers with faster emission intervals can carry a proportionally larger burst allowance.
- Verify connection-affinity assumptions against real batch-client traffic patterns. nginx's round-robin is per-request in the current config, but long-lived batch client connections might still concentrate traffic on one node if upstream keepalive and client connection reuse interact unexpectedly.
- Give the peer coordinator's proposer automatic failover via a majority-vote lease or Redis-backed lock (once Redis is provisioned). Currently, if the proposer node dies, rebalancing stops permanently until a human redeploys — safe (nodes keep their last-confirmed shares), but a liveness problem.
