# Quota observation and simulation

The proxy includes an opt-in Claude quota collector and deterministic simulation
worker for up to three accounts. The normal `cmd/server` binary records normalized
snapshots, estimates separate five-hour and weekly depletion times, and explains
hypothetical account/activation choices. Collection sends authenticated **GETs to
the OAuth usage endpoint only**, and also reads existing passive quota headers.
It never sends inference or activation prompts, refreshes credentials itself,
changes bindings, clears quota failures, or changes routing.

The standalone `cmd/quota-forecast` tool remains available for offline replay.
The simulation engine itself performs no network operations.

This implementation targets the fork's current `main`, not a backport to the
running v8.0.8 deployment (`fd48ea6840f5572deb53aeb5657740937ac9daaa`). It can be
reviewed and exercised entirely with synthetic data, without deploying a proxy.

## Enable the integrated worker

Use `observability.quota-forecast` in the server's private YAML configuration.
The commented section in `config.example.yaml` shows all settings. The minimum
configuration is:

```yaml
observability:
  quota-forecast:
    enabled: true
    model: your-configured-claude-model
    accounts:
      - {auth-id: synthetic-credential-A, alias: A}
      - {auth-id: synthetic-credential-B, alias: B}
      - {auth-id: synthetic-credential-C, alias: C}
    assumptions:
      demand: [{minutes: 360, units-per-hour: 30}]
```

Replace the synthetic IDs with the **existing auth manager credential IDs**,
available from authenticated `GET /v8/management/credentials` (`files[].id`).
Keep this mapping private. Choose the actual routed Claude model so model
availability, aliases, priority and quota failures are evaluated correctly.
Demand units are explained below; the example numbers are invented. Neither
account independence nor first-use timer activation is assumed by default.
Enabling observation does not require enabling either assumption.

Startup starts the worker; a configuration reload replaces it when settings
change. Setting `enabled: false` or shutting down cancels outstanding requests.
Only local Claude OAuth credentials are supported; API keys and Home-managed
credentials cannot be collected. Home mode skips the worker and logs a warning;
its existing management-route visibility is unchanged. The worker skips disabled
credentials and absent/expired access tokens;
the proxy's existing token refresh behavior is unchanged.

Default polling is once per minute (allowed range 30 seconds–one hour), with at
most one request in flight per account. A stalled request does not block other
accounts or stale-data reports. The worker adds no upstream deadlines. Failed
requests retain the last real observations and use the ordinary polling interval;
there is no immediate retry, refresh, paid fallback, or quota-error mutation.
Request-start time timestamps usage reads conservatively; passive headers keep
their original observation time. Missing resets remain unknown. Only an explicit
zero five-hour utilization with a null reset is marked reported idle, which
still does not verify first-use timer behavior.

## Read live forecasts and simulate a conversation

Both routes require the existing **management credential**, not a proxy API key:

```sh
curl -H "Authorization: Bearer $MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v8/management/observability/quota-forecast

curl -H "Authorization: Bearer $MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  -d '{"session_id":"your-canonical-session-id","pinned":"B"}' \
  http://127.0.0.1:8317/v8/management/observability/quota-forecast/simulate
```

The GET evaluates a new-conversation scenario against cached readings. The POST
optionally looks up the existing canonical session binding and/or accepts a pin
expressed as a configured alias. Omit `pinned` to evaluate session preservation,
and omit both fields for a new conversation. These requests do not fetch usage
or select an actual account. Lookup does not renew the affinity TTL. Ambiguous
or unsupported affinity causes abstention; a healthy binding outside the
configured pool is preserved without exposing its credential ID. The source
reports `enabled`, `mode`, collection state/source/observation timestamps,
`journal_error`, and a report with separate `measured` and `assumptions` fields.
Recommendations include plain-language reasons and unmet-demand comparisons
when the counterfactual is permitted. No routes are added under deprecated v0.
Simulation bodies must be a single JSON object of at most 8 KiB. Malformed
aliases or payloads return HTTP 400 without evaluating the scenario.

## Private persistence and recovery

By default, reports append to `<auth-dir>/quota-forecast/observations.jsonl`.
On Unix, files are mode 0600 and new directories are mode 0700. Other systems
require equivalent private filesystem permissions. Rotation bounds are 10 MiB
per file and three files **total**, configurable to 1–100 MiB and 1–10 files.
Optional `journal-path` must end in `.jsonl`; relative custom paths resolve from
the config directory. Persist the directory across container replacements.

Journals contain safe aliases, normalized measured quota, routing eligibility,
assumptions, collection status and recommendation reasons. They exclude raw
provider responses, credential IDs/tokens, emails, and session IDs. They are
still private usage records: do not commit or publish them. Aliases should not
encode personal information. The worker logs recommendation aliases/reasons,
so keep runtime logs private too. Journal failures are surfaced in status;
collection and cached simulations continue.

Restart restores the last two distinct measurements per alias from bounded
retained files. A one-way pool key prevents reuse when credential mappings or
the model change. Corrupt records are ignored; unreadable/oversize journals
report failed restoration and require a new baseline. Repeated readings do not
manufacture history, and restored stale readings remain stale. Rotation bounds
size rather than age; lowering the size bound discards oversized retained files
on the next write. Appends separate interrupted trailing records so subsequent
observations remain recoverable. This is local JSONL storage, not a durable database.

## Later deployment (not performed by this change)

Build a server image from this fork's merged commit using the existing Dockerfile:

```sh
docker build --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t llm-proxy:quota-forecast .
```

The existing upstream `eceasy/cli-proxy-api:latest` image does not contain this
fork feature. A later approved update must back up private configuration, pin
the fork image/commit, verify current-main configuration compatibility, and
recreate the proxy using its existing ports, auth and config volumes. Supply
the private ID mapping, routed model and explicit demand assumptions, then
enable the worker and inspect the authenticated GET after two polls. Keep the
previous image/config available for rollback. No deployment, restart, activation
experiment or paid usage is part of this implementation.

## Offline replay and file observation

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

The standalone observer keeps two distinct measurements per account in memory. Restarting
without historical samples requires two new measurements to recover burn rates.
To seed history, use saved reports' `measured` entries as scenario `samples`.
The journal includes all input measurements, assumptions, forecasts, comparisons,
and recommendation reasons. It is private usage data: keep it outside this
public repository, apply retention externally, and do not commit it. The tool
writes stdout; the caller controls file permissions and rotation.

Standalone watch mode records an externally maintained, normalized local
snapshot and does not contact providers. The normal proxy service uses the
integrated collector described above; it does not require a separate recorder.

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
- In offline input, `healthy` and `priority` are caller-supplied routing context for the requested
  model. Health must include credential/model availability, disabled state,
  quota rejections, cooldowns, and entitlement failures. Quota percentages do
  not establish credential health. Missing `healthy` is false.
- Optional `bound` and `pinned` are account pseudonyms supplied by the caller.
  A pin is authoritative. No actual session identifier is needed.

`Report.measured` retains the supplied snapshot and routing context. Offline
replays trust caller-supplied health/binding assertions. The integrated worker
reads the real auth manager's model availability and priority rules and its
read-only affinity lookup.

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
Equivalent timezone representations of the same reset instant are not conflicts.

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

The activation lead check projects both quota windows through their scheduled
resets, using the larger of measured burn and assumed demand in each minute.
A quiet first segment does not hide an approaching peak. A reset that prevents
depletion suppresses activation unless a limit depletes later within the lead
window; exhaustion exactly at a reset does not trigger activation.
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
- Integrated storage is bounded private JSONL; offline stdout storage remains
  caller-managed. Neither mode is a production scheduler. Model-specific usage
  buckets (such as additional weekly sublimits) and overflow are not collected;
  availability uses existing proxy state, not new entitlement probes.

Tests use synthetic measurements and explicit clocks, with no wall-clock sleeps.
They cover discontinuities, stale/repeated reads, missing values, separate weekly
exhaustion, zero burn, pins, affinity, priorities, deterministic replay, reset
rounding, activation cost, weekly bottlenecks, no-benefit ties, a conditional
benefit case, usage parsing, authenticated v8 routes, collector cancellation,
configuration reload, journal privacy/rotation/recovery, and local journal failures.

```sh
go test ./internal/quotaobserver ./internal/config ./internal/api ./sdk/cliproxy/...
go test -race ./internal/quotaobserver ./sdk/cliproxy/quotaforecast ./cmd/quota-forecast
go test ./...
gofmt -w .
go build -o test-output ./cmd/server && rm test-output
```
