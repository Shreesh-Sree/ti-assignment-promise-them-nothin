# Decisions — Promise Them Nothing Twice

## Conflict resolution

The two memos give directly contradictory orders against the same traffic: Priya requires a hard 429 at the contracted 300 RPM; Marcus requires Northwind never see a 429 while sustaining 800–1200 RPM nightly. Both memos contain an explicit escape hatch — Priya: *"commercial exceptions go through config and audit"*; Marcus: *"a temporary exception mechanism, invisible to the customer."* These converge on a single mechanism: an auditable, config-driven, time-scoped override that raises the effective limit, enforced identically for every customer by the same code path. The enforcement engine has no knowledge that Northwind exists.

**Rejected:** silent code-level bypass (Priya forbids by name); raising everyone's limit (Marcus didn't ask for that); telling Northwind to spread traffic (their ERP can't); queuing excess (81,000-request backlog arithmetic — see `DESIGN-NOTES.md` §"Queuing or buffering"); best-effort soft enforcement (reintroduces prior limiter #1's failure mode); fixed-wall-clock override window without grace (breaks most nights — see Part 1 §3).

**Unresolved gap:** a batch starting late can still outlive the override window. Padded with a grace period sized from the documented worst case; not fully eliminated.

## Technical design

**Algorithm:** GCRA (continuous spacing, one timestamp per customer per node). No fixed-window boundary bug. Provable worst case per rolling 60s window: `(ceil(quota/N) + burst) × N` where N=3 nodes, burst=1 — concretely 105 for a 100 RPM customer (due to share rounding), 1203 for Northwind at 1200 RPM (divides evenly).

**Coordination:** static per-node partition of the global limit, with an optional two-phase shrink-before-grow rebalancer behind the same `Coordinator` interface. Safety invariant: `sum(shares) ≤ quota` at every instant — proven in `DESIGN-NOTES.md` Part 2, verified against real captured timestamps in Parts 3–4.

**Burst tradeoff (adopted):** `coordinator.NodeBurst = 1`. Loosens the exact-quota guarantee by a small, named amount (not open-ended) in exchange for eliminating the false-reject problem that was losing 29–63% of compliant traffic at τ=0. Before/after on the same harness:

| Scenario | Burst=0 | Burst=1 |
|----------|---------|---------|
| northwind-batch reject % | 29.5% | 3.0% |
| window-boundary max/bound | 96/100 | 99/105 |
| over-limit-cutoff max/bound | 47/100 | 105/105 |
| node-failure | PASS (47/100) | PASS (47/105) |
| safety invariant | HOLDS | HOLDS |

## What the harness proves

Five named scenarios (`cmd/harness`), each PASS/FAIL from the output alone. The harness keeps its own client-side timestamps (independent of the server), computes a true rolling 60-second maximum per customer (not calendar-minute bucketing), and cross-checks against server logs without depending on them for the verdict.

A **fixed-window** limiter, tested by swapping in a deliberately broken counter (`make up-fixedwindow`, requires a `-tags fixedwindow` rebuild — the broken code is excluded from a normal `go build` entirely by a build tag, not just guarded by a warning), **fails** the harness when traffic happens to straddle a calendar-minute boundary during `over-limit-cutoff`: the 2x-at-boundary bug lets a node admit up to 2× its per-minute quota in any true 60-second rolling window, producing a max_roll_60s that exceeds the 105 bound. Whether a given run catches this depends on when within the minute the scenario starts; it is a probabilistic demonstration, not a guaranteed single-run failure. The GCRA limiter passes the same scenario in every run, with max_roll_60s landing between 103 and 105 (within the provable bound of 105) across the four runs checked.

**Not yet verified:** the window-boundary override-expiry gap, and zero-429 at 1200 RPM (residual ~3% from nginx multi-worker routing jitter at 150ms emission intervals — a config fix, not a code fix; see `DESIGN-NOTES.md` Part 4).

## If I had four more hours

- Resolve the ~3% northwind-batch residual by applying the headroom formula to the override ceiling in config (`P × (1 + T_sync/60)` above P99 peak).
- Key the override to observed batch activity instead of a fixed wall-clock window.
- Make burst tolerance per-tier (thread through `policy.Decision` like `Limit` already is).
- Verify connection-affinity assumptions against real batch-client traffic patterns.
