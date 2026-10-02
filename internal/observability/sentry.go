// Package observability exports only allowlisted operational metadata to Sentry.
// It deliberately does not integrate request capture, logrus, or breadcrumbs.
package observability

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

type Runtime struct {
	client            *sentry.Client
	hub               *sentry.Hub
	transport         *queuedTransport
	providers, models map[string]bool
	active            atomic.Int64
}

var current atomic.Pointer[Runtime]

func Enabled() bool { return current.Load() != nil }

// Start is startup-only. An invalid configuration disables telemetry without
// failing inference; callers should log only the fixed returned error message.
func Start(cfg config.SentryConfig) error {
	r, err := New(cfg, nil)
	if err != nil {
		return err
	}
	current.Store(r)
	return nil
}

// New accepts an in-memory transport for deterministic tests.
func New(cfg config.SentryConfig, transport sentry.Transport) (*Runtime, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.DSN == "" {
		cfg.DSN = os.Getenv("SENTRY_DSN")
	}
	if cfg.Environment == "" {
		cfg.Environment = os.Getenv("SENTRY_ENVIRONMENT")
	}
	if cfg.Release == "" {
		cfg.Release = os.Getenv("SENTRY_RELEASE")
	}
	if cfg.Release == "" {
		cfg.Release = buildinfo.Version + "+" + buildinfo.Commit
	}
	if cfg.Instance == "" {
		cfg.Instance, _ = os.Hostname()
	}
	if cfg.DSN == "" || math.IsNaN(cfg.TracesSampleRate) || math.IsInf(cfg.TracesSampleRate, 0) || cfg.TracesSampleRate < 0 || cfg.TracesSampleRate > 1 || cfg.QueueSize < 0 || cfg.QueueSize > 65536 || len(cfg.Models) > 256 || len(cfg.Providers) > 64 {
		return nil, errors.New("invalid Sentry configuration; telemetry disabled")
	}
	if _, err := sentry.NewDsn(cfg.DSN); err != nil {
		return nil, errors.New("invalid Sentry DSN; telemetry disabled")
	}
	if cfg.QueueSize == 0 {
		cfg.QueueSize = 256
	}
	r := &Runtime{providers: map[string]bool{}, models: map[string]bool{}}
	for _, v := range cfg.Providers {
		r.providers[v] = true
	}
	for _, v := range cfg.Models {
		r.models[v] = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	if transport == nil {
		transport = sentry.NewHTTPSyncTransport()
	}
	r.transport = &queuedTransport{inner: transport, queue: make(chan *sentry.Event, cfg.QueueSize), done: make(chan struct{}), cancel: cancel}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: cfg.DSN, Environment: cfg.Environment, Release: cfg.Release, ServerName: cfg.Instance,
		EnableTracing: true, TracesSampleRate: cfg.TracesSampleRate,
		SendDefaultPII: false, MaxSpans: 256, Transport: r.transport,
		HTTPClient:   &http.Client{Transport: &cancelTransport{ctx: ctx}},
		Integrations: func([]sentry.Integration) []sentry.Integration { return nil },
		BeforeSend:   scrubEvent, BeforeSendTransaction: scrubEvent,
		TraceIgnoreStatusCodes: [][]int{},
	})
	if err != nil {
		cancel()
		return nil, errors.New("Sentry initialization failed; telemetry disabled")
	}
	r.client = client
	r.hub = sentry.NewHub(client, sentry.NewScope())
	return r, nil
}

type cancelTransport struct {
	ctx    context.Context
	warned atomic.Bool
}

func (t *cancelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// No inference or telemetry network deadlines. Shutdown cancellation releases
	// a stalled telemetry connection after the flush budget has expired.
	response, err := http.DefaultTransport.RoundTrip(req.Clone(t.ctx))
	if (err != nil || (response != nil && response.StatusCode >= 400)) && t.ctx.Err() == nil && t.warned.CompareAndSwap(false, true) {
		log.Warn("Sentry delivery unavailable or rejected; telemetry may be lost")
	}
	return response, err
}

func Shutdown(ctx context.Context) bool {
	r := current.Swap(nil)
	if r == nil {
		return true
	}
	return r.Shutdown(ctx)
}

func (r *Runtime) Shutdown(ctx context.Context) bool {
	if r == nil {
		return true
	}
	ok := r.client.FlushWithContext(ctx)
	r.client.Close()
	return ok
}

func (r *Runtime) provider(v string) string {
	if r.providers[v] {
		return v
	}
	return "other"
}
func (r *Runtime) model(v string) string {
	if r.models[v] {
		return v
	}
	return "other"
}

type stateKey struct{}
type Request struct {
	mu                          sync.Mutex
	r                           *Runtime
	span                        *sentry.Span
	ctx                         context.Context
	start                       time.Time
	provider, requested, served string
	stream                      bool
	failure                     string
	attempts                    int
	failedSpans                 []*sentry.Span
	finished                    bool
	reported                    bool
	exhausted                   bool
	ttft                        time.Duration
	timingKind                  string
}

func FromContext(ctx context.Context) *Request {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(stateKey{}).(*Request)
	return s
}

type inheritedContext struct {
	context.Context
	telemetry context.Context
}

func (c inheritedContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.telemetry.Value(key)
}

// Inherit retains a handler's existing cancellation behavior while carrying
// telemetry across handlers that intentionally start with context.Background.
func Inherit(parent, request context.Context) context.Context {
	if s := FromContext(request); s != nil && FromContext(parent) == nil {
		return inheritedContext{parent, s.ctx}
	}
	return parent
}

// Begin accepts normalized routes and the existing request ID. It continues
// only sentry-trace, not untrusted baggage (which can contain arbitrary data).
func Begin(ctx context.Context, route, method, requestID, trace string) (context.Context, *Request) {
	r := current.Load()
	if r == nil {
		return ctx, nil
	}
	return r.begin(ctx, route, method, requestID, trace)
}

func (r *Runtime) begin(ctx context.Context, route, method, requestID, trace string) (context.Context, *Request) {
	hub := r.hub.Clone()
	ctx = sentry.SetHubOnContext(ctx, hub)
	span := sentry.StartTransaction(ctx, method+" "+route, sentry.WithOpName("http.server"), sentry.WithTransactionSource(sentry.SourceRoute), sentry.ContinueFromHeaders(trace, ""))
	span.SetTag("route", route)
	span.SetTag("method", method)
	// Request IDs are correlation data, never metric dimensions.
	span.SetData("request_id", requestID)
	s := &Request{r: r, span: span, start: span.StartTime, provider: "other", requested: "other", served: "other"}
	ctx = context.WithValue(span.Context(), stateKey{}, s)
	s.ctx = ctx
	return ctx, s
}

// Begin is the runtime-scoped equivalent of the process-wide Begin function.
func (r *Runtime) Begin(ctx context.Context, route, method, requestID, trace string) (context.Context, *Request) {
	if r == nil {
		return ctx, nil
	}
	return r.begin(ctx, route, method, requestID, trace)
}

func ConfigureRequest(ctx context.Context, model string, stream bool) {
	s := FromContext(ctx)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.requested = s.r.model(model)
	if stream && !s.stream {
		s.stream = true
		s.gaugeActive(s.r.active.Add(1))
	}
}

func Outcome(ctx context.Context, err error, status int) {
	s := FromContext(ctx)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.failure = Category(err, status)
}

func Category(err error, status int) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var coded interface{ StatusCode() int }
	if status == 0 && errors.As(err, &coded) {
		status = coded.StatusCode()
	}
	switch {
	case status == 429:
		return "rate_limit"
	case status == 401 || status == 403:
		return "authentication"
	case status >= 500:
		return "upstream_5xx"
	case status >= 400:
		return "request_error"
	case err != nil:
		var network interface{ Timeout() bool }
		if errors.As(err, &network) {
			return "connection"
		}
		return "internal"
	default:
		return "success"
	}
}

func (s *Request) attrs(outcome string) []attribute.Builder {
	return []attribute.Builder{attribute.String("provider", s.provider), attribute.String("model", s.requested), attribute.String("outcome", outcome), attribute.Bool("stream", s.stream), attribute.String("route", s.span.Tags["route"]), attribute.String("method", s.span.Tags["method"])}
}

func (s *Request) meter() sentry.Meter { return sentry.NewMeter(s.ctx) }
func (s *Request) gaugeActive(n int64) {
	sentry.NewMeter(sentry.SetHubOnContext(context.Background(), s.r.hub)).Gauge("llm.streams.active", float64(n))
}

func (s *Request) Finish(status int, cancelled bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	outcome := s.failure
	if outcome == "" || outcome == "success" {
		outcome = Category(nil, status)
	}
	if cancelled {
		outcome = "cancelled"
	}
	if outcome == "upstream_5xx" && s.attempts == 0 && (s.failure == "" || s.failure == "success") {
		outcome = "internal"
	}
	duration := time.Since(s.start)
	opts := sentry.WithAttributes(s.attrs(outcome)...)
	s.meter().Count("llm.requests", 1, opts)
	s.meter().Distribution("llm.request.duration", duration.Seconds(), opts, sentry.WithUnit(sentry.UnitSecond))
	if s.ttft > 0 {
		name := "llm.request.first_byte"
		if s.timingKind == "token" {
			name = "llm.request.ttft"
		}
		s.meter().Distribution(name, s.ttft.Seconds(), opts, sentry.WithUnit(sentry.UnitSecond))
	}
	if s.stream {
		s.gaugeActive(s.r.active.Add(-1))
		s.meter().Count("llm.streams", 1, opts)
		s.meter().Distribution("llm.stream.duration", duration.Seconds(), opts, sentry.WithUnit(sentry.UnitSecond))
	}
	if s.attempts > 1 && outcome != "success" && outcome != "cancelled" {
		s.meter().Count("llm.retries.exhausted", 1, opts)
	}
	s.span.SetTag("outcome", outcome)
	s.span.SetTag("provider", s.provider)
	s.span.SetTag("requested_model", s.requested)
	s.span.SetTag("served_model", s.served)
	s.span.SetTag("stream", strconv.FormatBool(s.stream))
	s.span.SetData("http.response.status_code", status)
	s.span.SetData("duration_ms", float64(duration)/float64(time.Millisecond))
	if s.ttft > 0 {
		s.span.SetData("first_response_ms", float64(s.ttft)/float64(time.Millisecond))
		s.span.SetTag("timing_kind", s.timingKind)
	}
	for _, sp := range s.failedSpans {
		sp.SetData("another_attempt_succeeded", outcome == "success")
	}
	if outcome == "success" {
		s.span.Status = sentry.SpanStatusOK
	} else if outcome == "cancelled" {
		s.span.Status = sentry.SpanStatusCanceled
	} else {
		s.span.Status = sentry.SpanStatusInternalError
		if !s.reported && outcome != "request_error" && (s.attempts > 0 || outcome == "internal" || outcome == "upstream_5xx" || outcome == "connection" || outcome == "credential_exhausted") {
			s.capture("terminal_request", outcome, nil)
		}
	}
	s.span.Finish()
}

// Panic never formats the recovered value: panic strings can contain secrets.
func Panic(ctx context.Context) {
	s := FromContext(ctx)
	if s == nil {
		r := current.Load()
		if r == nil {
			return
		}
		s = &Request{ctx: sentry.SetHubOnContext(ctx, r.hub.Clone()), provider: "other", requested: "other"}
	}
	s.mu.Lock()
	if s.reported || s.finished {
		s.mu.Unlock()
		return
	}
	s.failure = "internal"
	s.capture("recovered_panic", "internal", sentry.NewStacktrace())
	isWebsocket := s.span != nil && s.span.Tags["method"] == "WS"
	s.mu.Unlock()
	// HTTP middleware owns HTTP finalization; socket turns have no such
	// finalizer if their forwarding handler panics.
	if isWebsocket {
		s.Finish(http.StatusInternalServerError, false)
	}
}

func (s *Request) capture(kind, category string, stack *sentry.Stacktrace) {
	s.reported = true
	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Fingerprint = []string{kind, category, s.provider}
	if kind == "recovered_panic" {
		event.Fingerprint = []string{"{{ default }}", kind}
	}
	event.Tags = map[string]string{"kind": kind, "category": category, "provider": s.provider, "model": s.requested}
	event.Exception = []sentry.Exception{{Type: kind, Value: category, Stacktrace: stack}}
	if hub := sentry.GetHubFromContext(s.ctx); hub != nil {
		hub.CaptureEvent(event)
	}
}

// scrubEvent is defense in depth; producers only construct allowlisted data.
func scrubEvent(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.Request = nil
	e.User = sentry.User{}
	e.Breadcrumbs = nil
	e.Attachments = nil
	e.Modules = nil
	e.Threads = nil
	e.Message = ""
	for i := range e.Exception {
		if st := e.Exception[i].Stacktrace; st != nil {
			for j := range st.Frames {
				st.Frames[j].Vars = nil
				st.Frames[j].PreContext = nil
				st.Frames[j].PostContext = nil
				st.Frames[j].ContextLine = ""
				st.Frames[j].AbsPath = ""
			}
		}
	}
	for key := range e.Contexts {
		if key != "trace" {
			delete(e.Contexts, key)
		}
	}
	return e
}
