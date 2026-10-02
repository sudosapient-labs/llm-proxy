package observability

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"
)

type Attempt struct {
	once            sync.Once
	s               *Request
	span            *sentry.Span
	ctx             context.Context
	provider, model string
	start           time.Time
	status          int
}

type attemptKey struct{}

// UpstreamStatus observes a response code at the existing HTTP timing hook.
func UpstreamStatus(ctx context.Context, status int) {
	if ctx == nil {
		return
	}
	a, _ := ctx.Value(attemptKey{}).(*Attempt)
	if a == nil {
		return
	}
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	a.status = status
}

func BeginAttempt(ctx context.Context, provider, model string) (context.Context, *Attempt) {
	s := FromContext(ctx)
	if s == nil {
		return ctx, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return ctx, nil
	}
	provider, model = s.r.provider(provider), s.r.model(model)
	s.attempts++
	span := sentry.StartSpan(ctx, "llm.upstream")
	span.SetTag("provider", provider)
	span.SetTag("model", model)
	span.SetData("attempt", s.attempts)
	if s.attempts > 1 {
		reason := "previous_attempt_failed"
		if len(s.failedSpans) > 0 {
			reason = s.failedSpans[len(s.failedSpans)-1].Tags["category"]
		}
		span.SetTag("retry_reason", reason)
		kind := "retry"
		if s.provider != provider || s.served != model {
			kind = "fallback"
		}
		span.SetTag("retry_kind", kind)
		s.meter().Count("llm.retries", 1, sentry.WithAttributes(attribute.String("provider", provider), attribute.String("model", model), attribute.String("reason", reason), attribute.String("kind", kind)))
	}
	s.provider, s.served = provider, model
	// Preserve execution-specific context values while attaching the child span.
	ctx = span.Context()
	a := &Attempt{s: s, span: span, provider: provider, model: model, start: span.StartTime}
	ctx = context.WithValue(ctx, attemptKey{}, a)
	a.ctx = ctx
	return ctx, a
}

func (a *Attempt) Finish(err error, status int) {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if status == 0 {
			var coded interface{ StatusCode() int }
			if errors.As(err, &coded) {
				status = coded.StatusCode()
			}
		}
		a.s.mu.Lock()
		defer a.s.mu.Unlock()
		if status == 0 {
			status = a.status
		}
		category := Category(err, status)
		a.span.SetTag("category", category)
		if status != 0 {
			a.span.SetData("http.response.status_code", status)
		}
		if category == "success" {
			a.span.Status = sentry.SpanStatusOK
		} else if category == "cancelled" {
			a.span.Status = sentry.SpanStatusCanceled
		} else {
			a.span.Status = sentry.SpanStatusInternalError
			// Bound retained span references independently of retry configuration.
			if len(a.s.failedSpans) < 256 {
				a.s.failedSpans = append(a.s.failedSpans, a.span)
			}
		}
		opts := sentry.WithAttributes(attribute.String("provider", a.provider), attribute.String("model", a.model), attribute.String("category", category))
		meter := sentry.NewMeter(a.ctx)
		meter.Count("llm.attempts", 1, opts)
		meter.Distribution("llm.attempt.duration", time.Since(a.start).Seconds(), opts, sentry.WithUnit(sentry.UnitSecond))
		a.span.Finish()
	})
}

// RetryWait measures scheduling waits separately from execution.
func RetryWait(ctx context.Context) func() {
	s := FromContext(ctx)
	if s == nil {
		return func() {}
	}
	span := sentry.StartSpan(s.ctx, "llm.retry.wait")
	return func() {
		sentry.NewMeter(s.ctx).Distribution("llm.retry.wait", time.Since(span.StartTime).Seconds(), sentry.WithUnit(sentry.UnitSecond))
		span.Finish()
	}
}

// Usage exports numeric values only; the caller must not pass an entire usage
// record. A zero here means unknown and is not exported as a measured zero.
func Usage(ctx context.Context, provider, model, served string, started time.Time, ttft time.Duration, timingKind string, buckets map[string]int64) {
	r := current.Load()
	if r == nil {
		return
	}
	provider, model = r.provider(provider), r.model(model)
	if s := FromContext(ctx); s != nil {
		s.mu.Lock()
		if !s.finished {
			if served != "" {
				s.served = r.model(served)
			}
			if s.ttft == 0 && ttft > 0 && !started.IsZero() {
				s.ttft = started.Add(ttft).Sub(s.start)
				s.timingKind = timingKind
			}
		}
		s.mu.Unlock()
	}
	ctx = sentry.SetHubOnContext(ctx, r.hub)
	meter := sentry.NewMeter(ctx)
	if ttft > 0 {
		name := "llm.attempt.first_byte"
		if timingKind == "token" {
			name = "llm.attempt.ttft"
		}
		meter.Distribution(name, ttft.Seconds(), sentry.WithUnit(sentry.UnitSecond), sentry.WithAttributes(attribute.String("provider", provider), attribute.String("model", model)))
	}
	for _, bucket := range []string{"input_uncached", "input_cache_read", "input_cache_write", "output_non_reasoning", "output_reasoning", "unclassified"} {
		if value := buckets[bucket]; value > 0 {
			meter.Count("llm.tokens", value, sentry.WithAttributes(attribute.String("provider", provider), attribute.String("model", model), attribute.String("bucket", bucket)))
		}
	}
}

// FirstResponse is called synchronously at the existing timing hook so a late
// usage publication cannot shorten the trace or erase its timing observation.
func FirstResponse(ctx context.Context, when time.Time, kind string) {
	s := FromContext(ctx)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.finished && when.After(s.start) {
		s.ttft = when.Sub(s.start)
		s.timingKind = kind
	}
}

func Refresh(ctx context.Context, provider string, duration time.Duration, err error, actionable bool) {
	r := current.Load()
	if r == nil {
		return
	}
	provider = r.provider(provider)
	ctx = sentry.SetHubOnContext(ctx, r.hub.Clone())
	category := Category(err, 0)
	opts := sentry.WithAttributes(attribute.String("provider", provider), attribute.String("outcome", category))
	meter := sentry.NewMeter(ctx)
	meter.Count("llm.credentials.refresh", 1, opts)
	meter.Distribution("llm.credentials.refresh.duration", duration.Seconds(), opts, sentry.WithUnit(sentry.UnitSecond))
	if actionable && category != "cancelled" {
		s := &Request{ctx: ctx, provider: provider, requested: "other"}
		s.capture("credential_refresh", category, nil)
	}
}

func CredentialExhausted(ctx context.Context) {
	s := FromContext(ctx)
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.failure = "credential_exhausted"
	if s.exhausted {
		return
	}
	s.exhausted = true
	s.meter().Count("llm.credentials.exhausted", 1)
}

// StreamFailure reports semantic failures independently of committed headers.
func StreamFailure(ctx context.Context, err error) { Outcome(ctx, err, 0) }
