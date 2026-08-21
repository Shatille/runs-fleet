---
concept: Absent Is Not Zero
last_compiled: 2026-08-21
topics_connected: [observability, admin-ui, state-storage, housekeeping]
status: active
---

# Absent Is Not Zero

## Pattern

Every number runs-fleet reports about itself is derived — from a metric series, a
sampled rollup, a filtered table scan. Each of those can fail to answer, and the
zero value of the Go type that carries the answer is `0`. Rendering that `0` is
the cheapest possible code path and the most expensive possible mistake: a
measured zero and an unmeasured quantity look identical on a dashboard, and the
reader has no way to tell which one they are looking at.

The codebase has converged on treating "could not observe" as a distinct state
that must survive all the way to the presentation layer — as a second return
value, an omitted field, a `nil` result, or an explicit `unavailable` string —
rather than collapsing into the zero value at the point of failure.

The inverse failure is what taught the lesson. PR #403 filtered completed jobs on
a status string that the store never wrote, so the query matched almost nothing
and the Cost tab reported **1 job / 0.0 hours** where the truth was **8,102 jobs
/ 432.3 hours**. Nothing errored. The page rendered a confident, precise,
entirely fabricated number for weeks.

## Instances

- **2026-08-21** in [observability](../topics/observability.md): `spotInterruptionCount`
  ([pkg/cost/reporter.go](../../pkg/cost/reporter.go)) returns `(int, bool)`. The
  metric only exists when the CloudWatch backend publishes it, and that backend
  now ships disabled, so an absent series is the *common* case — the report prints
  `Spot interruptions: unavailable (CloudWatch metrics disabled)`. A genuinely
  measured zero still prints `0`; the two are no longer the same output.
- **2026-08-21** in [observability](../topics/observability.md): `ComputeFleetMTDIn`
  ([pkg/cost/fleetmtd.go:68](../../pkg/cost/fleetmtd.go)) returns `(nil, nil)`
  when no day in the period has a rollup. Deliberately not a zeroed struct: a
  zero fleet cost rendered beside a non-zero attributed cost reads as "the fleet
  has no overhead", which is the exact claim the feature exists to refute.
- **2026-08-21** in [admin-ui](../topics/admin-ui.md): the Cost tab's Fleet Cost
  card is *omitted* rather than zeroed when the sampler has produced nothing, so
  a page that has never been sampled cannot display `$0.00`.
- **2026-08-21** in [housekeeping](../topics/housekeeping.md): the fleet-cost
  sampler's `busyInstanceSet` ([pkg/housekeeping/fleet_cost.go](../../pkg/housekeeping/fleet_cost.go))
  returns `nil` when the busy-instance lookup fails, and the caller then
  attributes *nothing* for that tick. Attributing everything would have erased
  the unattributed gap the sampler measures; attributing nothing understates a
  number already labelled an estimate.
- **2026-08** in [state-storage](../topics/state-storage.md): `HasLiveInstanceClaim`
  fails **closed** — an unreadable claim row counts as held. Here the safe
  direction is the opposite (assume presence, not absence), which is the point:
  the rule is not "prefer zero" or "prefer non-zero" but "decide which way the
  unknown must fall, and say so".
- **2026-08-21** in [housekeeping](../topics/housekeeping.md): a tick that clamps
  a long gap at `fleetCostMaxElapsed` marks its day `partial`, so a total known
  to understate is reported as understating rather than as complete.

## What This Means

The discipline costs a return value, a struct field, or a `nil` check per
call site — and it is the difference between a number a reader can act on and a
number that quietly lies. Three properties make it work:

1. **The unknown state must be carried, not inferred.** A caller cannot
   reconstruct "the query failed" from a `0` after the fact. `(int, bool)` and
   `(*T, nil)` are the mechanisms; `omitempty` on the wire is the third.
2. **Omission beats zero at the presentation layer.** A missing card prompts
   "why is that not there?"; a `$0.00` card answers a question nobody asked,
   wrongly. The admin UI renders the fleet block only when it exists.
3. **The direction of the failure is a design decision.** `busyInstanceSet`
   attributes nothing; `HasLiveInstanceClaim` assumes held. Both are correct
   because both were chosen against a specific consequence — under-reporting a
   cost estimate versus terminating an instance a job is using.

The pattern is not universally applied, and the gaps are worth knowing. The
Prometheus and Datadog backends still emit histograms CloudWatch no-ops, so a
metric absent from one backend is present in another — "unavailable" is
backend-specific. And `cmd/agent` unconditionally zeroes `timings.boot` and
`timings.config` after a successful config find
([cmd/agent/main.go:101-104](../../cmd/agent/main.go)); with `omitempty` those two
bootstrap phases are now *never* reported. The zeroing is deliberate and
commented — it keeps standby wait out of the bootstrap total — but the effect is
that absence of those fields no longer distinguishes "old agent" from "current
agent by design", which is the same ambiguity this pattern exists to remove.

## Sources

- [observability](../topics/observability.md)
- [admin-ui](../topics/admin-ui.md)
- [state-storage](../topics/state-storage.md)
- [housekeeping](../topics/housekeeping.md)
