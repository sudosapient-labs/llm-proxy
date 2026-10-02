# Quota observation and simulation

`quota-forecast` is a local, observation-only tool for a pool of up to three
Claude accounts. It records normalized snapshots, estimates separate five-hour
and weekly depletion times, and explains hypothetical account/activation
choices. It never contacts a provider, loads credentials or proxy configuration,
sends activation prompts, changes bindings, or changes routing. It has no server
startup hook and adds no management endpoint.

This implementation targets the fork's current `main`, not a backport to the
running v8.0.8 deployment (`fd48ea6840f5572deb53aeb5657740937ac9daaa`). It can be
reviewed and exercised entirely with synthetic data, without deploying a proxy.

## Replay and periodic observation

Replay the committed **synthetic** example; stdout is a JSONL journal with one
report per sample:

```sh
go run ./cmd/quota-forecast -input docs/examples/quota-forecast.json
```

For periodic recording, supply a local sample file using the shape of one entry
in `samples`, and a scenario file with assumptions and optionally historical
samples. A separate, trusted observation source must update the sample file by
atomic rename. Start the recorder explicitly:

```sh
umask 077
go run ./cmd/quota-forecast -input /private/scenario.json \
  -watch /private/current-sample.json -interval 1m >> /private/quota-journal.jsonl
```

Watch mode uses the current clock for `at` but preserves each account's
`observed_at`. Repeated polls retain the last real burn estimate and eventually
become stale; polling is not evidence of new quota. Replay uses `sample.at` as a
controllable clock. Neither mode sleeps inside the forecast/simulation engine.
Ctrl-C stops watching. Read/schema/output failures stop the process with a
nonzero exit; they do not silently continue with an apparently fresh report.

The observer keeps two distinct measurements per account in memory. Restarting
without historical samples requires two new measurements to recover burn rates.
To seed history, use saved reports' `measured` entries as scenario `samples`.
The journal includes all input measurements, assumptions, forecasts, comparisons,
and recommendation reasons. It is private usage data: keep it outside this
public repository, apply retention externally, and do not commit it. The tool
writes stdout; the caller controls file permissions and rotation.

This first version does **not** install a collector in the proxy or poll a live
usage endpoint. Watch mode records an externally maintained, normalized local
snapshot. This avoids coupling the feature to deprecated management APIs or to
production credential handling. There is no automatic live data source in the
committed example.

## Input and passive signals

Use short pseudonyms such as `A`, `B`, and `C`, never emails, credential filenames,
organization IDs, or session IDs. Each complete pool snapshot contains:

- `at`: evaluation/recording time, RFC3339; monotonic during replay.
- `accounts`: one to three uniquely named accounts, each with its actual
  `observed_at`, `five_hour`, and `weekly` windows. Utilization is a **fraction
  from 0 to 1**, not percentage points. A null window/utilization means unknown.
- Each known running window has `reset_at`, an absolute RFC3339 timestamp.
  Only an explicitly reported idle five-hour window may use
  `{"utilization":0,"idle":true}`. A missing reset alone never means idle.
- `healthy` and `priority` are caller-supplied routing context for the requested
  model. Health must include credential/model availability, disabled state,
  quota rejections, cooldowns, and entitlement failures. Quota percentages do
  not establish credential health. Missing `healthy` is false.
- Optional `bound` and `pinned` are account pseudonyms supplied by the caller.
  A pin is authoritative. No actual session identifier is needed.

`Report.measured` retains the supplied snapshot and routing context; it does not
claim to have independently verified the caller's health/binding assertions.

For an embedding that already has passive observations, the SDK adapter accepts
only a header map and returns normalized windows:

```go
five, weekly := quotaforecast.ClaudeWindows(auth.Quota.Signals)
// Assign these to a pseudonymized Account and preserve auth.Quota.ObservedAt.
// Supply health/priority separately; do not serialize auth or its metadata.
```

It recognizes `anthropic-ratelimit-unified-{5h,7d}-{utilization,reset}`. Header
utilizations are fractions and resets are Unix seconds. Unrecognized headers
are discarded; malformed, nonfinite, or conflicting values become unknown.
Headers cannot establish idle timers. A usage-API source using percentages must
normalize them by dividing by 100 and explicitly identify an idle window.
The adapter performs no network access. The latest passive snapshot in
`auth/quota_signals.go` is replaced per response; sample it frequently enough to
capture distinct readings, and never replace its observation time with poll time.

## Forecast and recommendation rules

Burn is the utilization delta divided by elapsed hours between the two latest
distinct observations, separately for each limit. Both must share the same reset
and have a gap no larger than `max_age_minutes`. A changed reset, utilization
decrease, or missing data breaks the estimate until a new comparable pair exists.
Duplicate polls do not add history. Conflicting values at the same observation
time, out-of-order input, future measurements, or malformed normalized input
reject the whole sample without partially updating history.

Positive burn gives `(1 - utilization) / burn`, adjusted for elapsed time since
the observation. Zero burn has no finite depletion estimate; it is not a promise
of unlimited capacity. `reset_before_depletion` means the linear extrapolation
crosses the current window boundary. An expired reset requires another measured
snapshot; forecasts do not silently refill it. These are point estimates, not
confidence intervals. Independent weekly and five-hour rates are reported even
when one dominates.

For conversation recommendations:

1. Preserve a healthy explicit pin; abstain if the pinned account is absent or
   unavailable. Never suggest a different account for a pin.
2. Preserve a healthy binding across priorities and forecast uncertainty. A
   fresh exhausted limit in a current window or caller-reported failure allows hypothetical
   failover. Do not refresh or create an actual affinity binding.
3. For a new conversation, require fresh complete windows, positive five-hour
   capacity, weekly reserve, and sufficient runway. Estimate conservative runway
   using the larger of measured burn and the **peak supplied demand**, assigning
   the entire team demand to that candidate. Reserve is subtracted from weekly
   capacity. Choose highest eligible priority, then longest runway, then alias.
   Missing burn history may use the explicitly supplied demand assumption;
   missing/stale capacity cannot. Future resets are not credited for eligibility.

This preserves the existing selector's healthy-binding precedence and pin
semantics. Production's sliding one-hour affinity TTL and disabled cooling are
not changed. Quota error handling remains in the existing auth manager. No
CLIProxyAPIHome behavior or integration is changed.

## Simulation assumptions

All assumptions are serialized separately from the supplied observations. The
example deliberately sets both `independent_accounts` and
`first_use_starts_timer` to false. Activation comparison requires **both** to be
true. Setting them true permits a counterfactual; it does not verify them.
Shared organization constraints remain unknown, and first-use timer activation
has not been experimentally established.

| Input | Meaning |
| --- | --- |
| `max_age_minutes` | Maximum snapshot age and maximum interval for estimating burn (1–1440). |
| `minimum_runway_minutes` | Required conservative runway for new conversations (0–2880). |
| `activation_lead_minutes` | Consider activation only when an active account approaches depletion (0–2880). |
| `weekly_reserve` | Fraction of weekly quota reserved when admitting new work or activating an idle account. |
| `weekly_units_per_unit` | Assumed weekly percentage-point cost per five-hour percentage point, greater than zero and at most 100. |
| `activation_cost` | Hypothetical prompt cost in five-hour percentage points, from zero to less than 100; also consumes weekly quota. |
| `demand` | Consecutive segments with integer `minutes` and nonnegative `units_per_hour`; total horizon 1–2880 minutes. |

One workload unit consumes one percentage point of a five-hour budget on every
account; weekly consumption uses the supplied conversion. Successful token
counts are not treated as quota units. Demand is an explicit whole-team workload
scenario, not a fitted historical workload. The example's numbers are invented.

The activation lead check considers demand segments throughout the lead window
and measured burn, so a quiet first segment does not hide an approaching peak.
The simulation uses one-minute steps and rounds reset times **up** to a step
boundary. It starts both policies from the same projected current quota and
actual future reset timestamps. After five-hour resets, it assumes an idle
window that starts on the next use; weekly resets repeat after seven days. It
serves the current bound account until capacity runs out, then chooses by
priority and alias. Quota is consumed subject to **both** limits. Known unhealthy
accounts are excluded, with no assumed recovery. Missing/stale/expired windows
on a healthy account suppress the comparison. Explicit pins also suppress it.

The wait policy starts idle timers only when ordinary demand reaches an account.
Each early policy starts exactly one eligible idle account **now**, charges the
activation cost, and uses the same subsequent policy as waiting. A recommendation
requires strictly fewer unmet units (tolerance `1e-8`); ties favor waiting. The
report retains both unmet-demand totals for every compared account. An earlier
reset timestamp is not the objective. Activation is a returned recommendation,
never a network action. Weekly reserve controls admission, not a hard quota
floor: existing simulated demand can consume the reserve to avoid blocking.

## Limits and validation

- Account independence and timer mechanics are unverified. Shared organization,
  model-specific limits, credits, entitlement constraints, and paid overflow are
  not modeled. No paid fallback is assumed.
- The pooled simulation approximates fungible demand and one active binding; it
  does not model the team's concurrent sticky conversations or cache effects.
  Per-conversation binding preservation is a separate recommendation rule.
- A two-point rate is sensitive to rounding, bursts, off-proxy activity, and
  delayed provider reports. Stale observations cause abstention for new work,
  not fabricated replenishment. Caller-reported healthy bindings are retained.
- Equal five-hour capacities and a constant weekly cost conversion are explicit
  simplifications. Weekly reset and fixed-window mechanics are scenario models.
- Unmet demand is dropped, not queued. Results depend on horizon and demand
  shape; a short-horizon benefit may disappear over a longer horizon. There is
  no claim of optimality, sustainable capacity, or latency improvement.
- Observation storage is a caller-managed JSONL journal, not a database or a
  production scheduler. No activation experiment, deployment, or live usage is
  part of this change.

Tests use synthetic measurements and explicit clocks, with no wall-clock sleeps.
They cover discontinuities, stale/repeated reads, missing values, separate weekly
exhaustion, zero burn, pins, affinity, priorities, deterministic replay, reset
rounding, activation cost, weekly bottlenecks, no-benefit ties, a conditional
benefit case, and local journal failures.

```sh
go test ./sdk/cliproxy/quotaforecast ./cmd/quota-forecast
go test -race ./sdk/cliproxy/quotaforecast ./cmd/quota-forecast
go test ./...
gofmt -w .
go build -o test-output ./cmd/server && rm test-output
```
