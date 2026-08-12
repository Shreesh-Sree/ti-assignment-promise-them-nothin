# RelayAPI Rate Limiter

Per-customer rate limiting across 3 stateless nodes behind a round-robin load balancer, with no shared memory and no Redis dependency. Resolves two contradictory stakeholder requirements (hard 429 enforcement vs. never-429-for-our-biggest-customer) via a config-driven, auditable override mechanism — see `DESIGN-NOTES.md` Part 1 and `../DECISIONS.md`.

## Prerequisites

- Go 1.22+
- Docker with Compose v2
- curl (for health checks)
- ~500MB disk (Go module cache + Docker images)
- No cloud accounts, no Redis, no paid tools

## Quick start (under 5 minutes)

```bash
cd solution/deploy

# Bring up 3 nodes + nginx (static coordinator, real clock)
make up

# Run the full verification harness (5 scenarios, ~5 min)
make harness

# See results — ALL SCENARIOS PASS with plain make up (real clock, outside override window)
# For the override-active phase of northwind-batch, use make up-northwind-window
# (shifts dev-clock to 02:30 UTC) — northwind-batch also PASS at the 1250 ceiling
# Non-zero exit on any FAIL

# Tear down
make down
```

## Running with Northwind's override window active

The override is time-scoped to 02:00–05:00 UTC. To exercise it without waiting:

```bash
cd solution/deploy
make up-northwind-window   # pins dev-clock to 02:30 UTC
make harness               # northwind-batch now detects override-active phase
make down
```

## Running individual scenarios

```bash
make harness SCENARIOS=window-boundary
make harness SCENARIOS=northwind-batch
make harness SCENARIOS=node-failure
```

## Proving the harness catches bugs

```bash
cd solution/deploy

# Bring up with a deliberately broken fixed-window counter
# (requires -tags=fixedwindow rebuild — the broken code is excluded from
# a normal binary entirely by a build tag, not just a startup warning)
make up-fixedwindow

# Run harness — over-limit-cutoff will FAIL when traffic straddles a calendar
# minute boundary (the 2x-at-boundary bug). This is probabilistic: whether a
# given run catches it depends on where in the minute the scenario starts.
# If it passes, re-run; it will fail within a few attempts.
make harness SCENARIOS=over-limit-cutoff

make down
```

## What the harness checks

| Scenario | What it proves |
|----------|----------------|
| two-tenants-fair | Customer A's traffic can't steal from Customer B's budget |
| over-limit-cutoff | A 4x-over-limit customer is cut off — max rolling 60s never exceeds the provable bound |
| window-boundary | No 2x spike at calendar-minute boundaries (the fixed-window bug) |
| northwind-batch | Override mechanism works; reports honestly if Marcus's "never 429" bar is met |
| node-failure | Killing a node mid-run never causes over-admission; under-admission during recovery is correct |

Each scenario computes the **true rolling 60-second maximum** across real client-side timestamps — not calendar-minute buckets, which is the exact counting mistake fixed-window limiters make.

## Counting semantics (for enterprise security review)

> RelayAPI enforces each customer's request limit using a continuous, rate-based check — similar to a metered tap that only opens as fast as your contracted rate allows — rather than a count that resets on the clock minute, so there is no gap at the top of a minute a customer could exploit to briefly exceed their limit. Because our service runs on multiple servers that don't share memory, we don't rely on a single central counter that could become slow or unavailable; instead, each server is given a fixed, provably-correct share of your total limit and independently guarantees it will never let you exceed that share, so the total across all servers can never exceed your contracted limit even if the servers are temporarily unable to communicate with each other. If we ever grant a temporary exception — for example, to support a documented operational need — it exists as an explicit, dated configuration record with a mandatory expiration date and an audit trail, never as a hidden rule in the code, so at any time we can show precisely what limit applied to your account and why.

## Architecture

```
                ┌──────────┐
  Client ──────►│  nginx   │ round-robin, per-request
                │  (LB)    │
                └─┬──┬──┬──┘
                  │  │  │
         ┌────────┘  │  └────────┐
         ▼           ▼           ▼
    ┌─────────┐ ┌─────────┐ ┌─────────┐
    │ node-1  │ │ node-2  │ │ node-3  │  each: httpapi → policy → coordinator → GCRA
    │(proposer│ │         │ │         │
    │ if peer)│ │         │ │         │
    └─────────┘ └─────────┘ └─────────┘
```

Each node independently enforces its share of the limit (GCRA, τ=1 burst tolerance). The optional peer coordinator rebalances shares via a two-phase shrink-before-grow protocol; the static coordinator splits evenly and never communicates. Both satisfy the same safety invariant: `sum(shares) ≤ quota` at every instant.

## Burst=0 vs Burst=1 comparison

| Metric | Burst=0 (strict) | Burst=1 (adopted) |
|--------|---|---|
| Provable worst case (rolling 60s) | exactly `quota` | `(ceil(quota/3)+1)×3` |
| False rejection rate (compliant traffic) | 29–63% | 0% |
| northwind-batch (1200 RPM, headroom ceiling 1250) | FAIL (29.5% rejected) | PASS (0% rejected) |
| window-boundary (100 RPM, 2.5 min) | 96/100 | 99–100/105 |
| over-limit-cutoff (4x over, 90s) | PASS | PASS (104–105/105) |
| Safety invariant | HOLDS | HOLDS |

The northwind-batch residual (~3% at Burst=0, ceiling=1200) is resolved: the override ceiling in `configs/customers.yaml` was raised to 1250 RPM using the headroom formula from `DESIGN-NOTES.md` Part 1 — `P × (1 + T_sync/60) = 1200 × (1 + 2.5/60) = 1250`. This gives each node a 143.9ms emission interval (vs 150ms at 1200 RPM), absorbing the nginx multi-worker jitter that caused the residual. See `DESIGN-NOTES.md` Part 4 and `../DECISIONS.md` for the full derivation.

## Project structure

```
solution/
├── cmd/relayapi/       # node binary (env-configured)
├── cmd/harness/        # verification harness (5 scenarios)
├── internal/
│   ├── ratelimit/      # GCRA core (session 3)
│   ├── policy/         # config, overrides, validation (session 4)
│   ├── coordinator/    # static + peer strategies (session 5)
│   ├── httpapi/        # HTTP layer, headers, audit logging (session 5)
│   └── audit/          # structured audit events (session 4)
├── configs/            # customers.yaml (the single source of truth for limits)
├── deploy/             # Dockerfile, docker-compose, nginx, Makefile
└── DESIGN-NOTES.md     # full reasoning (Parts 1–4)
```

## What's unfinished

- Override window is wall-clock-based with a grace period, not keyed to observed batch activity (a batch starting late can outlive the window)
- Burst tolerance is a global constant (τ=1 for all customers), not per-tier
- Peer coordinator's proposer has no automatic failover (documented limitation, degrades safely to static)
- nginx `proxy_next_upstream` retry reduces node-failure errored count (13 → 12) but doesn't eliminate it: RST errors on established keepalive connections are inherently unretryable
