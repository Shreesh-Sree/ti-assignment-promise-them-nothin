# Working context

## What this is
Rate limiter for RelayAPI. Take-home. Two stakeholder memos conflict on
purpose and the conflict is the graded part.

## The resolution, already decided, do not relitigate
One effective limit per customer, resolved from config as a function of
(customer, time), enforced identically for everyone. Northwind gets a dated,
expiring override in config. The enforcement engine does not know Northwind
exists.

## Hard rules
- No branching on customer ID anywhere in the request path. Config only.
- Reject direction only. If nodes disagree we under-admit, never over-admit.
- No network call on the request path. Coordination is a background goroutine.
- Every override application writes a structured audit event.
- Overrides must have an expiry. Config fails to start without one.
- No time.Sleep in tests. Injected Clock everywhere.

## Constraints from the brief that are easy to forget
- 3 stateless nodes, round robin, no session affinity, no shared memory.
- Redis may not be available. Ops will not provision new infra.
- Northwind: 300 RPM contracted, 800 to 1200 actual, 02:00 to 04:00 UTC,
  90 to 120 minutes, aggressive retry on 429.
- Reviewer must clone and run this in under 15 minutes with free tools.
- Two prior limiters died here: one under-enforced across nodes, one had
  boundary correctness bugs under load.

## Definitions
"Never exceeds quota" means: max admitted across any rolling 60 second
window is at most quota + burst. Not per calendar minute. Per calendar
minute is the fixed-window bug.

## Style
Go, standard library heavy. Minimal deps. Interfaces at the consumer.
Concrete types returned. Errors wrapped with context.