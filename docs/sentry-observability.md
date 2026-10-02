# Sentry observability

Sentry is the only telemetry destination. No dashboard, alert, project, or other
remote resource is provisioned by this change. Organization access is needed for
the setup below. Telemetry is optional and **disabled by default**.

## Enable and operate

Create a Go Sentry project with Application Metrics and tracing enabled. Copy its
DSN into `observability.sentry.dsn` in the v8 YAML layout (legacy root `sentry` is
also accepted). Enable `observability.sentry.enabled`, set `environment`, and
populate `models` and `providers` with explicit trusted operational names.
Unlisted names become `other`, including on traces. Provider lists are limited
to 64 entries and model lists to 256; never allow arbitrary client model names.
OpenAI-compatible executors can use configured provider names, so add those
trusted names explicitly; the built-in Vertex provider is named `vertex`.

Use `SENTRY_DSN`, `SENTRY_ENVIRONMENT`, and `SENTRY_RELEASE` as fallbacks for empty
YAML values. `.env` is loaded by the existing startup code. Without an override,
release is `buildinfo.Version+buildinfo.Commit`; service instance is the hostname
(override using `instance`). Configuration is **startup-only**: hot reload does
not reinitialize Sentry or alter in-flight traces. Restart after changing any
Sentry setting. Home config updates likewise require a restart for this block.
The SDK configuration type also re-exports `SentryConfig` for embedded services.

`traces-sample-rate` is a fraction in [0,1]. Start with 1 in staging, then choose a
production budget (the example uses 0.1). Zero disables transaction export, not
metrics or issues. Incoming valid `sentry-trace` headers continue distributed
traces; Sentry's parent sampling decision can override the local rate. Untrusted
`baggage` is intentionally ignored. This implements Sentry propagation, not W3C
`traceparent` continuation. Telemetry headers are removed from core upstream
options; HTTP telemetry middleware also removes them from incoming headers
before inference. No provider requests gain telemetry headers. CORS/gateway
configuration for trusted callers must allow `sentry-trace` if cross-origin
continuation is required.

Invalid DSNs, sample rates, queue sizes, or excessive allowlist sizes disable
telemetry with a fixed warning, not an inference startup failure. A missing DSN
is invalid when enabled. Debug/automatic request capture, default PII, source
context, breadcrumbs, logrus hooks, body attachments, and default SDK
integrations are not enabled. Exported fields are explicitly constructed from
route/method, existing request ID (trace data only), allowlisted model/provider,
status, outcome/category, attempt index/retry kind, timestamps/durations, token
buckets, release/environment/instance, and sanitized code stack frames.
No usage record is serialized: API keys, credential IDs/metadata, token hashes,
headers, bodies, sessions, URLs/query parameters, prompts, completions, cookies,
and error/panic values are excluded. Do not place secrets in operator-supplied
deployment metadata or allowlists.

The event/batch queue has one sender and a configurable capacity (default 256,
maximum 65536). The SDK additionally uses a bounded metric batch queue (100
items in v0.49.0). Overflow drops telemetry without waiting for network I/O;
Sentry delivery/rate limiting also remains best effort. Queue overflow and
delivery failure/rejection each log a fixed, once-per-runtime warning without
exception text, destination URLs, or credentials. Inference threads do
bounded local work only. No inference or telemetry network deadlines are added.
Service shutdown flushes the SDK metric batches and the event queue using the
existing shutdown context, then closes/cancels telemetry transport resources.
An incomplete flush warns of possible loss. Crash shutdown cannot guarantee
delivery. The local queue's drop counter is diagnostic (`queuedTransport.dropped`)
and is not itself a remotely reliable health metric. Counts are independent of
trace sampling, **not an exact durable ledger**. Compare traffic with proxy logs
or your ingress if you need to detect telemetry loss; no second backend is added.

## Verified SDK capabilities

Checked official docs and the pinned `github.com/getsentry/sentry-go v0.49.0`
implementation while implementing this integration:

- [Application Metrics](https://docs.sentry.io/platforms/go/metrics/): standalone
  counters, gauges, distributions since v0.42.0, unaffected by trace sampling.
- [Tracing](https://docs.sentry.io/platforms/go/tracing/) and
  [custom instrumentation](https://docs.sentry.io/platforms/go/tracing/instrumentation/custom-instrumentation/):
  transactions, child spans, status/data attributes; spans are exported when the
  transaction finishes. Custom attributes are not independent aggregates.
- [Shutdown and draining](https://docs.sentry.io/platforms/go/configuration/draining/):
  `FlushWithContext`/`Flush` then `Close`; flush can fail and data can be lost.

This SDK has no public transaction custom-measurement setter. Duration/timing
attributes on traces are the closest supported drill-down, while standalone
distributions provide percentiles independently of trace sampling. Availability,
retention, ingestion quotas, metric alert support, and dashboard formulas depend
on your Sentry organization/version/plan; verify them before rollout. Older
self-hosted Sentry deployments may not support Application Metrics envelopes.

## Outcome matrix

All `llm.*` metrics below are Application Metrics (not sampled span counts).
Transactions/child spans are sampled; issue capture is independent of trace
sampling. Positive numeric usage observations only are exported; unknown values
are absent, never synthesized as zero.

| Value | Code source | Sentry destination | Aggregation / coverage limitation |
|---|---|---|---|
| Logical traffic and final outcome | `internal/logging/sentry.go`; handler lifecycle completion | `llm.requests` counter with outcome, route, method, stream, provider, requested model | One final observation per HTTP inference request; not one per retry. Success, cancellation, request/auth/rate-limit/upstream/internal/exhaustion categories are distinct. |
| Request ID, method, route, HTTP status, requested/served model, deployment | `internal/observability/sentry.go`; existing Gin request ID; usage response model | Transaction data/tags, release/environment/server_name | IDs are not Sentry trace IDs and never metric labels. Served model uses upstream execution model, refined by reported response model when available. No bodies parsed by telemetry middleware. WS status is semantic, not a second HTTP response. |
| Total latency p50/p95/p99 | Middleware finalizer / WS forwarder return | `llm.request.duration` distribution, transaction timestamps and `duration_ms` | Includes retries, waiting, downstream stream forwarding. HTTP errors before execution still count. |
| Token TTFT vs byte latency | `UsageReporter.ObserveTokenEvent`/`setTTFT`/publication | `llm.request.ttft`, `llm.attempt.ttft`; alternatively `llm.request.first_byte`, `llm.attempt.first_byte` | Logical first-response duration includes time before the selected attempt; attempt timing uses reporter observation. Verified protocol token detection: Codex Responses SSE/WS; compatible plugin helpers can also report token events. Most other providers measure first byte/packet, **not** token TTFT. Meta's stream uses round-trip-only timing but does not call the token observer, so missing timing is omitted. |
| Upstream attempts, category, duration | Core auth `executeObserved` / `streamObserved`, including Home and credits execution; HTTP reporter round-trip hook | `llm.attempts`, `llm.attempt.duration`; `llm.upstream` child span | Attempt duration runs through chunk-channel completion, not connection opening. Status comes from the HTTP reporter response or errors implementing `StatusCode`; bypassing these hooks leaves it unknown (not invented as 200). An attempt is one core executor invocation: executor-internal HTTP resends/subrequests are not separate attempt spans, and preflight executor errors can appear as failed attempts. A transport error exposing a network error interface is `connection`; other statusless errors are `internal`. |
| Rate limits, auth, 5xx, connection errors | Sanitized `Category`, attempt error status | Filter `llm.attempts` by category | Expected retries do not create issues. A 429 followed by success is a failed attempt and a successful request. |
| Retry/fallback counts and reasons | Next `BeginAttempt`; existing cooldown wait | `llm.retries` by kind/reason; `llm.retry.wait` distribution + wait span | Fallback means a different allowlisted provider or execution model; credential changes are retries. Reason is prior sanitized failure category. Unlisted names collapse to `other`; this can hide cross-model/provider fallback distinctions. Home busy/admission waits outside `waitForCooldown` are not separately timed. |
| Retry exhaustion and later success | Request finalizer | `llm.retries.exhausted`; failed span `another_attempt_succeeded` | Exhaustion inferred from final failure after >1 attempts, not an authoritative retry-budget decision. Single-attempt terminal failures remain in request outcomes. Up to 256 spans retained; counters still cover additional attempts. |
| Stream duration/completion/interruption and active streams | Request configuration, finalizer, stream error sends/lifecycle; WS forwarder | `llm.stream.duration`, `llm.streams` by outcome, `llm.streams.active` gauge | Lifetime ends after handler/WS turn forwarding returns. Post-HTTP-200 errors are semantic failures. Client cancellations are separate. Gauge is process-local; do not sum successive samples, use latest per service instance and sum across instances if supported. |
| Input/output/cache/reasoning tokens | `helps.UsageReporter.publishRecord`; canonical `TokenBreakdown.Valid` | `llm.tokens` counter by non-overlapping bucket | Buckets: input_uncached, input_cache_read, input_cache_write, output_non_reasoning, output_reasoning, unclassified. Sum all once for total; do not add inclusive input/output totals again. Includes reported failed-attempt usage and multi-model usage; not guaranteed billing totals. Invalid/missing breakdown exports only reported total as unclassified. |
| Credential refresh health | Core `refreshObserved` | `llm.credentials.refresh`, `llm.credentials.refresh.duration`; actionable credential issue | Core auto/manual/unauthorized refresh paths. No IDs or credential metadata. Only unauthorized/invalid-grant failures create refresh issues; cancellation is not an issue. Provider-internal/plugin refresh flows bypassing core are not covered. |
| No eligible credentials | Handler completion of core auth_not_found/auth_unavailable errors | `llm.credentials.exhausted`, final `credential_exhausted` outcome + issue | Based on typed final errors. Exhaustion masked by a preferred earlier upstream error is represented by that upstream outcome instead. |
| Panics/internal/terminal errors | Existing Gin recovery callback; finalizer; refresh hook | Sanitized exception issues with stable fingerprints | Panic values omitted; code stack retained without source context/locals. Panics have stack-based default grouping. One report per recovered inference panic; no log hook/executor issue duplication. Request errors, expected preflight auth/rate-limit rejections, intermediate 429s, retries and cancellations do not create issues. |

HTTP instrumentation covers POST inference routes in `/v1`, `/v1beta`,
`/openai/v1`, and `/backend-api/codex`, including Gemini action paths and SSE.
Model listing, health, management and WebSocket handshakes are excluded.
The Responses WebSocket handler traces each accepted inference turn, not the
socket lifetime; malformed/rejected messages before execution are not counted.
Core outbound Codex WebSocket execution is included in upstream stream attempts.
AI Studio/wsrelay ingress frames, Amp reverse-proxy inference, direct SDK calls
without an inbound telemetry context, and alternate plugin-owned executions do
not have complete logical/attempt tracing. Plugins using `UsageReporter` can
still export numeric token/timing observations; plugin-owned attempts are not
invented from usage records. Extending those ingress contracts is future work.
No CLIProxyAPIHome wire contracts/log forwarding were changed; counterpart Home
changes are not required for these local hooks.

## Dashboard setup (manual in your Sentry organization)

Select the project and environment in Application Metrics; first verify the
named metrics arrive from staging. Create a dashboard with the following metric
queries/widgets. Use time-series rates where applicable and consistent windows.
Metric UI query/formula syntax varies by Sentry version; the names, filters and
aggregations below specify the required queries rather than claiming a deployed
dashboard JSON/API artifact.

| View | Query / aggregation |
|---|---|
| Traffic and final health | Sum `llm.requests`, grouped by outcome; successful/total, failed/total, cancelled/total. Failure excludes success and cancelled. |
| End-to-end latency | p50, p95, p99 of `llm.request.duration`; group by route, stream, provider/model. |
| Token responsiveness | p50, p95, p99 of `llm.request.ttft`; separate panels for `llm.request.first_byte`, never combine them. |
| Provider/model comparison | Group `llm.attempts` by provider/model/category; p95 `llm.attempt.duration` and available attempt TTFT/first-byte distributions. |
| Retry and fallback behavior | Sum `llm.retries` by kind/reason; sum `llm.retries.exhausted`; p95 `llm.retry.wait`. Drill into sampled attempt spans. |
| Streaming health | Sum `llm.streams` by outcome, completion/success rate, cancellation rate, interruption/failure rate; p95 stream duration; latest active gauge per instance. |
| Token usage | Sum `llm.tokens` by bucket/provider/model. Input = three input buckets; output = two output buckets; reasoning is a subset of output, not additional output. |
| Credential health | Sum `llm.credentials.refresh` by outcome/provider; p95 refresh duration; sum `llm.credentials.exhausted`. |
| Release regressions | Filter/group metrics by `sentry.release` and environment; Issues views for kind=terminal_request, recovered_panic, credential_refresh; release comparison and new/regressed issues. |

If your organization lacks cross-metric formulas, use adjacent filtered and
total counter panels (same window), rather than calling sampled transaction
throughput an exact failure rate. If metric alerts are unavailable, retain issue
alerts for credential exhaustion/internal failures and explicitly treat this as
a limitation; do not pretend sampled transaction alerts cover unsampled traffic.

Suggested **illustrative**, operator-adjustable alerts:

- Final failure rate >5% for 5 minutes, minimum 100 requests/window (exclude
  cancellations); group by environment, optionally provider/model.
- p95 token TTFT >3 seconds for 10 minutes, minimum 50 measured observations;
  use a separate first-byte alert for providers without token detection.
- `llm.credentials.exhausted` sum >0 over 5 minutes; route to credential operators.
- New or regressed `recovered_panic` / internal terminal issues in production,
  grouped by release; configure issue ownership and notification destinations.
- Optional: refresh failures >10% over 10 minutes with a minimum sample volume;
  stream interruption rate >5%, excluding cancellations.

Deploy with a staging DSN, induce a successful request, a retry, a post-200
failure, cancellation, and a harmless panic in a test environment, then verify
metrics/traces/issues in the organization. Check outbound envelopes for privacy
before production. Remote resource existence and alert delivery remain manual
verification steps, not claims made by this implementation.

## Local validation

Focused tests passed for observability, logging, configuration, auth conductor,
API handlers, OpenAI handlers, and executor usage helpers. The observability and
logging packages passed race tests; the new auth wrapper tests also passed a
focused race run. These use local/in-memory transports and cover disabled
configuration, successful and terminal requests, retry success, post-200 stream
failure, cancellation, panic deduplication/privacy, unsampled metrics, and
transport failure/queue saturation.

`gofmt -w .`, `git diff --check`, and the required
`go build -o test-output ./cmd/server && rm test-output` succeeded.

The broader `go test -p 2 ./...` run was not fully green: the untouched
`internal/client/codex/live` test `TestPionMediaRelayBridgesAudioAndDataChannel`
failed because the upstream DataChannel was not created. The executor package
also failed in that run, but its complete isolated rerun passed; the original
executor failure's test name was not recovered from truncated output. Earlier
Home/auth failures also passed subsequent package reruns. These results do not
establish that the full suite is green. No remote Sentry ingestion, dashboards,
or alert delivery were validated; those remain the manual staging checks above.
