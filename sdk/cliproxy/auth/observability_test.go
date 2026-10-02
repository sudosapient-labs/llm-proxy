package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/observability"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type observationTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (*observationTransport) Configure(sentry.ClientOptions) {}
func (m *observationTransport) SendEvent(e *sentry.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}
func (*observationTransport) Flush(time.Duration) bool              { return true }
func (*observationTransport) FlushWithContext(context.Context) bool { return true }
func (*observationTransport) Close()                                {}

func TestObservedExecutorRetryAndStreamCompletion(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "post_200_failure", true: "cancelled"}[cancelled], func(t *testing.T) {
			transport := &observationTransport{}
			r, err := observability.New(config.SentryConfig{Enabled: true, DSN: "https://public@example.invalid/1", TracesSampleRate: 1, QueueSize: 32, Providers: []string{"claude"}, Models: []string{"test-model"}}, transport)
			if err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, request := r.Begin(parent, "/v1/responses", "POST", "existing-id", "")
			observability.ConfigureRequest(ctx, "test-model", true)
			auth := &Auth{Provider: "claude"}
			chunks := make(chan cliproxyexecutor.StreamChunk)
			exec := &claudeCancellationTestExecutor{executeFn: func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
				return cliproxyexecutor.Response{}, &Error{HTTPStatus: 429, Message: "secret-rate-limit-body"}
			}, streamFn: func(context.Context, *Auth) (*cliproxyexecutor.StreamResult, error) {
				return &cliproxyexecutor.StreamResult{Chunks: chunks, Headers: http.Header{"X-Test": []string{"unchanged"}}}, nil
			}}
			if _, err := executeObserved(ctx, exec, auth, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{}, false); err == nil {
				t.Fatal("failed attempt changed")
			}
			result, err := streamObserved(ctx, exec, auth, cliproxyexecutor.Request{Model: "test-model"}, cliproxyexecutor.Options{})
			if err != nil || result.Headers.Get("X-Test") != "unchanged" {
				t.Fatal("stream result changed")
			}
			delivered := make(chan struct{})
			go func() { chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("secret-completion")}; close(delivered) }()
			chunk := <-result.Chunks
			if string(chunk.Payload) != "secret-completion" {
				t.Fatal("payload changed")
			}
			<-delivered
			transport.mu.Lock()
			for _, e := range transport.events {
				if e.Type == "transaction" {
					t.Error("trace finished before stream")
				}
			}
			transport.mu.Unlock()
			terminal := errors.New("secret-upstream-error")
			if cancelled {
				cancel()
				for range result.Chunks {
				}
				close(chunks)
				observability.Outcome(ctx, context.Canceled, 0)
			} else {
				go func() { chunks <- cliproxyexecutor.StreamChunk{Err: terminal}; close(chunks) }()
				if got := <-result.Chunks; got.Err != terminal {
					t.Fatal("error changed")
				}
				for range result.Chunks {
				}
				observability.Outcome(ctx, terminal, 502)
			}
			request.Finish(200, cancelled)
			flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer flushCancel()
			if !r.Shutdown(flushCtx) {
				t.Fatal("flush failed")
			}
			transport.mu.Lock()
			defer transport.mu.Unlock()
			var trace *sentry.Event
			counts := 0
			for _, e := range transport.events {
				if e.Type == "transaction" {
					trace = e
				}
				for _, m := range e.Metrics {
					if m.Name == "llm.attempts" {
						counts++
					}
				}
			}
			if trace == nil || len(trace.Spans) != 2 || counts != 2 {
				t.Fatalf("attempt coverage incomplete: trace=%+v counts=%d", trace, counts)
			}
			want := "upstream_5xx"
			if cancelled {
				want = "cancelled"
			}
			if trace.Tags["outcome"] != want {
				t.Fatalf("outcome=%s", trace.Tags["outcome"])
			}
		})
	}
}

func TestTelemetryHeadersAreNotForwarded(t *testing.T) {
	headers := http.Header{"Sentry-Trace": []string{"trace"}, "Baggage": []string{"secret"}, "Traceparent": []string{"trace"}, "Tracestate": []string{"state"}, "Authorization": []string{"Bearer unchanged"}}
	got := telemetryOptions(cliproxyexecutor.Options{Headers: headers})
	if got.Headers.Get("Sentry-Trace") != "" || got.Headers.Get("Baggage") != "" || got.Headers.Get("Traceparent") != "" || got.Headers.Get("Tracestate") != "" {
		t.Fatal("telemetry headers forwarded")
	}
	if got.Headers.Get("Authorization") != "Bearer unchanged" || headers.Get("Baggage") != "secret" {
		t.Fatal("input/upstream authentication mutated")
	}
}
