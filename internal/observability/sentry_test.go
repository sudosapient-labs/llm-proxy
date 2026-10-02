package observability

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type memoryTransport struct {
	mu               sync.Mutex
	events           []*sentry.Event
	entered, release chan struct{}
	fail             bool
}

func (*memoryTransport) Configure(sentry.ClientOptions) {}
func (m *memoryTransport) SendEvent(e *sentry.Event) {
	if m.entered != nil {
		select {
		case m.entered <- struct{}{}:
		default:
		}
		<-m.release
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}
func (m *memoryTransport) Flush(time.Duration) bool              { return !m.fail }
func (m *memoryTransport) FlushWithContext(context.Context) bool { return !m.fail }
func (*memoryTransport) Close()                                  {}
func (m *memoryTransport) snapshot() []*sentry.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*sentry.Event(nil), m.events...)
}

func testRuntime(t *testing.T, rate float64, transport *memoryTransport, size int) *Runtime {
	t.Helper()
	r, err := New(config.SentryConfig{Enabled: true, DSN: "https://public@example.invalid/1", TracesSampleRate: rate, QueueSize: size, Providers: []string{"codex", "claude"}, Models: []string{"test-model"}, Instance: "test-instance", Release: "test-release", Environment: "test"}, transport)
	if err != nil {
		t.Fatal(err)
	}
	old := current.Swap(r)
	t.Cleanup(func() { current.Store(old) })
	return r
}

func flush(t *testing.T, r *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !r.Shutdown(ctx) {
		t.Fatal("flush failed")
	}
}

func metrics(events []*sentry.Event, name string) []sentry.Metric {
	var result []sentry.Metric
	for _, e := range events {
		for _, m := range e.Metrics {
			if m.Name == name {
				result = append(result, m)
			}
		}
	}
	return result
}

func transaction(events []*sentry.Event) *sentry.Event {
	for _, e := range events {
		if e.Type == "transaction" {
			return e
		}
	}
	return nil
}

type statusError int

func (s statusError) Error() string   { return "secret upstream body Bearer secret-token" }
func (s statusError) StatusCode() int { return int(s) }

func TestDisabledAndInvalid(t *testing.T) {
	r, err := New(config.SentryConfig{}, nil)
	if r != nil || err != nil {
		t.Fatal("disabled telemetry must be inert")
	}
	for _, cfg := range []config.SentryConfig{{Enabled: true}, {Enabled: true, DSN: "secret-invalid"}, {Enabled: true, DSN: "https://public@example.invalid/1", TracesSampleRate: 2}, {Enabled: true, DSN: "https://public@example.invalid/1", QueueSize: -1}} {
		r, err = New(cfg, nil)
		if r != nil || err == nil || strings.Contains(err.Error(), "secret-invalid") {
			t.Fatalf("invalid config not sanitized: %v", err)
		}
	}
}

func TestRequestOutcomesIndependentOfSampling(t *testing.T) {
	for _, rate := range []float64{0, 1} {
		for _, tc := range []struct {
			name              string
			err               error
			status            int
			stream, cancelled bool
			outcome           string
		}{
			{"success", nil, 200, false, false, "success"},
			{"terminal", statusError(503), 503, false, false, "upstream_5xx"},
			{"post_headers_failure", statusError(502), 200, true, false, "upstream_5xx"},
			{"client_cancellation", context.Canceled, 200, true, true, "cancelled"},
		} {
			t.Run(tc.name+string(rune('0'+int(rate))), func(t *testing.T) {
				transport := &memoryTransport{}
				r := testRuntime(t, rate, transport, 32)
				ctx, req := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
				ConfigureRequest(ctx, "test-model", tc.stream)
				_, attempt := BeginAttempt(ctx, "codex", "test-model")
				attempt.Finish(tc.err, 0)
				Outcome(ctx, tc.err, 0)
				req.Finish(tc.status, tc.cancelled)
				req.Finish(tc.status, tc.cancelled)
				flush(t, r)
				events := transport.snapshot()
				counts := metrics(events, "llm.requests")
				if len(counts) != 1 {
					t.Fatalf("logical counts = %d", len(counts))
				}
				if counts[0].Attributes["outcome"].AsString() != tc.outcome {
					t.Fatalf("outcome = %v", counts[0].Attributes)
				}
				if len(metrics(events, "llm.attempts")) != 1 {
					t.Fatal("attempt count missing")
				}
				if rate == 0 && transaction(events) != nil {
					t.Fatal("unsampled trace exported")
				}
				if rate == 1 {
					tr := transaction(events)
					if tr == nil || tr.Tags["outcome"] != tc.outcome {
						t.Fatal("trace outcome missing")
					}
					if tr.Release != "test-release" || tr.ServerName != "test-instance" {
						t.Fatal("deployment metadata missing")
					}
				}
				issues := 0
				for _, e := range events {
					if len(e.Exception) > 0 {
						issues++
					}
				}
				want := 0
				if tc.outcome == "upstream_5xx" {
					want = 1
				}
				if issues != want {
					t.Fatalf("issues=%d want=%d", issues, want)
				}
			})
		}
	}
}

func TestFailedAttemptThenSuccess(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 1, m, 32)
	ctx, s := r.begin(context.Background(), "/v1/chat/completions", "POST", "existing-id", "0123456789abcdef0123456789abcdef-0123456789abcdef-1")
	ConfigureRequest(ctx, "test-model", false)
	_, first := BeginAttempt(ctx, "codex", "test-model")
	first.Finish(statusError(429), 0)
	wait := RetryWait(ctx)
	wait()
	_, second := BeginAttempt(ctx, "claude", "test-model")
	second.Finish(nil, 0)
	Outcome(ctx, nil, 200)
	s.Finish(200, false)
	flush(t, r)
	events := m.snapshot()
	tr := transaction(events)
	if tr == nil || tr.Tags["outcome"] != "success" || len(tr.Spans) != 3 {
		t.Fatalf("trace=%+v", tr)
	}
	if tr.Spans[0].Data["another_attempt_succeeded"] != true {
		t.Fatal("retry recovery missing")
	}
	if tr.Spans[0].TraceID.String() != "0123456789abcdef0123456789abcdef" {
		t.Fatal("distributed trace lost")
	}
	if tr.Spans[2].Tags["retry_kind"] != "fallback" {
		t.Fatal("fallback missing")
	}
	if len(metrics(events, "llm.requests")) != 1 || len(metrics(events, "llm.attempts")) != 2 || len(metrics(events, "llm.retries")) != 1 {
		t.Fatal("attempt/request accounting incorrect")
	}
	for _, e := range events {
		if len(e.Exception) > 0 {
			t.Fatal("retry emitted an issue")
		}
	}
}

func TestPanicPrivacyAndDeduplication(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 1, m, 32)
	ctx, s := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
	ConfigureRequest(ctx, "secret-model", true)
	_, a := BeginAttempt(ctx, "secret-provider", "secret-model")
	a.Finish(errors.New("prompt secret-prompt https://secret-host/?token=secret-token"), 0)
	Panic(ctx)
	Panic(ctx)
	s.Finish(500, false)
	flush(t, r)
	issues := 0
	for _, e := range m.snapshot() {
		if len(e.Exception) > 0 {
			issues++
			if e.Exception[0].Stacktrace == nil {
				t.Fatal("panic stack missing")
			}
		}
		payload, err := json.Marshal(struct {
			Event   *sentry.Event
			Metrics []sentry.Metric
		}{e, e.Metrics})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), "secret-") {
			t.Fatalf("sensitive data exported: %s", payload)
		}
		for _, metric := range e.Metrics {
			for _, key := range []string{"request_id", "session_id", "auth_id", "url"} {
				if _, ok := metric.Attributes[key]; ok {
					t.Fatal("high cardinality label")
				}
			}
		}
	}
	if issues != 1 {
		t.Fatalf("panic issues=%d", issues)
	}
}

func TestUsageMissingValuesAndStreamingLifetime(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 0, m, 32)
	ctx, s := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
	ConfigureRequest(ctx, "test-model", true)
	Usage(ctx, "codex", "test-model", "", time.Time{}, 0, "token", nil)
	if s.finished || r.active.Load() != 1 {
		t.Fatal("stream finished before completion")
	}
	Usage(ctx, "codex", "test-model", "test-model", s.start, 250*time.Millisecond, "token", map[string]int64{"input_uncached": 8, "input_cache_read": 2, "output_non_reasoning": 3, "output_reasoning": 1, "unclassified": 0, "secret-bucket": 100})
	s.Finish(200, false)
	flush(t, r)
	if r.active.Load() != 0 {
		t.Fatal("active streams leaked")
	}
	if len(metrics(m.snapshot(), "llm.request.ttft")) != 1 || len(metrics(m.snapshot(), "llm.attempt.ttft")) != 1 {
		t.Fatal("TTFT missing or zero double counted")
	}
	tokens := metrics(m.snapshot(), "llm.tokens")
	sum := int64(0)
	for _, v := range tokens {
		n, _ := v.Value.Int64()
		sum += n
	}
	if sum != 14 {
		t.Fatalf("overlapping token accounting: %d", sum)
	}
}

func TestTransportSaturationAndFailureDoNotBlockRequests(t *testing.T) {
	m := &memoryTransport{entered: make(chan struct{}, 1), release: make(chan struct{}), fail: true}
	r := testRuntime(t, 1, m, 1)
	ctx, s := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
	Outcome(ctx, nil, 200)
	s.Finish(200, false)
	<-m.entered
	for range 8 {
		ctx, s = r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
		Outcome(ctx, nil, 200)
		s.Finish(200, false)
	}
	if r.transport.dropped.Load() == 0 {
		t.Fatal("queue did not drop")
	}
	close(m.release)
	ctxFlush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if r.Shutdown(ctxFlush) {
		t.Fatal("transport failure was not reported by flush")
	}
}

func TestInheritPreservesCancellationAndValues(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 0, m, 32)
	request, s := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
	parent, cancel := context.WithCancel(context.Background())
	ctx := Inherit(parent, request)
	if FromContext(ctx) != s || sentry.SpanFromContext(ctx) == nil {
		t.Fatal("telemetry was not inherited")
	}
	cancel()
	if ctx.Err() != context.Canceled {
		t.Fatal("cancellation behavior changed")
	}
	s.Finish(200, true)
	flush(t, r)
}

func TestCredentialHealthAndExhaustion(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 0, m, 32)
	ctx, request := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
	Refresh(ctx, "codex", time.Millisecond, nil, false)
	Refresh(ctx, "codex", 2*time.Millisecond, statusError(401), true)
	Refresh(ctx, "codex", 3*time.Millisecond, context.Canceled, true)
	CredentialExhausted(ctx)
	Outcome(ctx, statusError(503), 503)
	CredentialExhausted(ctx)
	request.Finish(503, false)
	flush(t, r)
	if len(metrics(m.snapshot(), "llm.credentials.refresh")) != 3 || len(metrics(m.snapshot(), "llm.credentials.refresh.duration")) != 3 {
		t.Fatal("refresh measurement missing")
	}
	if len(metrics(m.snapshot(), "llm.credentials.exhausted")) != 1 {
		t.Fatal("credential exhaustion duplicated")
	}
	if got := metrics(m.snapshot(), "llm.requests")[0].Attributes["outcome"].AsString(); got != "credential_exhausted" {
		t.Fatalf("exhaustion outcome = %s", got)
	}
	issues := 0
	for _, e := range m.snapshot() {
		if len(e.Exception) > 0 {
			issues++
		}
	}
	if issues != 2 {
		t.Fatalf("want actionable refresh + terminal issue, got %d", issues)
	}
}

func TestFirstByteIsNotTokenTTFTAndUpstreamStatusIsObserved(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 1, m, 32)
	ctx, request := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
	ConfigureRequest(ctx, "test-model", true)
	execCtx, attempt := BeginAttempt(ctx, "codex", "test-model")
	UpstreamStatus(execCtx, 201)
	attempt.Finish(nil, 0)
	FirstResponse(ctx, request.start.Add(200*time.Millisecond), "first_byte")
	Usage(ctx, "codex", "test-model", "", request.start, 100*time.Millisecond, "first_byte", nil)
	request.Finish(200, false)
	flush(t, r)
	if len(metrics(m.snapshot(), "llm.request.ttft")) != 0 || len(metrics(m.snapshot(), "llm.request.first_byte")) != 1 {
		t.Fatal("byte timing mislabeled as TTFT")
	}
	if value, _ := metrics(m.snapshot(), "llm.request.first_byte")[0].Value.Float64(); value != 0.2 {
		t.Fatalf("late usage overwrote observed logical timing: %v", value)
	}
	if got := transaction(m.snapshot()).Spans[0].Data["http.response.status_code"]; got != 201 {
		t.Fatalf("upstream status lost: %v", got)
	}
}

func TestExpectedPreflightRejectionsDoNotCreateIssues(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 0, m, 32)
	for _, status := range []int{400, 401, 403, 429} {
		_, request := r.begin(context.Background(), "/v1/responses", "POST", "existing-id", "")
		request.Finish(status, false)
	}
	flush(t, r)
	if len(metrics(m.snapshot(), "llm.requests")) != 4 {
		t.Fatal("rejected requests missing from aggregates")
	}
	for _, e := range m.snapshot() {
		if len(e.Exception) != 0 {
			t.Fatal("expected preflight rejection created an issue")
		}
	}
}

func TestNextRequestKeepsUnsampledMetricsIndependent(t *testing.T) {
	m := &memoryTransport{}
	r := testRuntime(t, 0, m, 32)
	ctx, first := r.Begin(context.Background(), "/v1/responses", "WS", "request-id", "")
	ConfigureRequest(ctx, "test-model", true)
	_, attempt := BeginAttempt(ctx, "codex", "test-model")
	attempt.Finish(nil, 200)
	first.Finish(200, false)
	ctx, second := first.Next(context.Background())
	if FromContext(ctx) != second || second == first || second.attempts != 0 || second.provider != "codex" {
		t.Fatal("successor reused previous request state or lost provider")
	}
	second.Finish(200, false)
	flush(t, r)
	if transaction(m.snapshot()) != nil || len(metrics(m.snapshot(), "llm.requests")) != 2 || len(metrics(m.snapshot(), "llm.streams")) != 2 {
		t.Fatal("unsampled successor accounting incorrect")
	}
	if r.active.Load() != 0 {
		t.Fatal("successor left an active stream")
	}
}
