package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/observability"
	"github.com/tidwall/gjson"
)

// Capture actual SDK envelopes, including metrics independent of transaction
// sampling. Call the returned function after the downstream handler has exited.
func captureWebsocketTelemetry(t *testing.T) func() []gjson.Result {
	t.Helper()
	var mu sync.Mutex
	var events []gjson.Result
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Error(errRead)
		}
		mu.Lock()
		defer mu.Unlock()
		for _, line := range strings.Split(string(body), "\n") {
			if gjson.Valid(line) {
				events = append(events, gjson.Parse(line))
			}
		}
	}))
	if errStart := observability.Start(config.SentryConfig{
		Enabled: true, DSN: "http://public@" + strings.TrimPrefix(sink.URL, "http://") + "/1",
		TracesSampleRate: 1, QueueSize: 128, Providers: []string{"codex", "xai"},
		Models: []string{"steering-test-model", "test-model", "xai-websocket-rollback-model"},
	}); errStart != nil {
		sink.Close()
		t.Fatal(errStart)
	}
	var once sync.Once
	finish := func() []gjson.Result {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if !observability.Shutdown(ctx) {
				t.Error("Sentry flush failed")
			}
			sink.Close()
		})
		mu.Lock()
		defer mu.Unlock()
		return append([]gjson.Result(nil), events...)
	}
	t.Cleanup(func() { finish() })
	return finish
}

func assertWebsocketTelemetryOutcomes(t *testing.T, events []gjson.Result, want []string) {
	t.Helper()
	var traces, requests, streams []string
	active := int64(-1)
	for _, event := range events {
		if event.Get("type").String() == "transaction" && event.Get("transaction").Exists() {
			traces = append(traces, event.Get("tags.outcome").String())
		}
		for _, metric := range event.Get("items").Array() {
			switch metric.Get("name").String() {
			case "llm.requests":
				requests = append(requests, metric.Get("attributes.outcome.value").String())
			case "llm.streams":
				streams = append(streams, metric.Get("attributes.outcome.value").String())
			case "llm.streams.active":
				active = metric.Get("value").Int()
			}
		}
	}
	for name, got := range map[string][]string{"traces": traces, "requests": requests, "streams": streams} {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s outcomes = %v, want %v", name, got, want)
		}
	}
	if active != 0 {
		t.Errorf("active streams = %d, want 0", active)
	}
}

func TestWebsocketTelemetryPreservesUpstreamFailureOnServerClose(t *testing.T) {
	for _, tc := range []struct {
		status  int
		outcome string
	}{
		{400, "request_error"}, {401, "authentication"}, {429, "rate_limit"}, {503, "upstream_5xx"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			capture := captureWebsocketTelemetry(t)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
			telemetry := newResponsesWebsocketTelemetry(c, c.Request.Context(), "test-model")
			_, attempt := observability.BeginAttempt(telemetry.ctx, "codex", "test-model")
			errUpstream := errors.New("secret upstream body")
			attempt.Finish(errUpstream, tc.status)
			telemetry.finish(c, &interfaces.ErrorMessage{StatusCode: tc.status, Error: errUpstream}, websocket.ErrCloseSent)
			telemetry.finish(c, nil, context.Canceled)
			events := capture()
			assertWebsocketTelemetryOutcomes(t, events, []string{tc.outcome})
			issues := 0
			for _, event := range events {
				if strings.Contains(event.Raw, "secret upstream body") {
					t.Error("upstream error body leaked into telemetry")
				}
				if event.Get("exception").Exists() {
					issues++
				}
				if event.Get("transaction").Exists() && event.Get("contexts.trace.data.http\\.response\\.status_code").Int() != int64(tc.status) {
					t.Error("upstream status was replaced by the socket close status")
				}
			}
			wantIssues := 1
			if tc.status == 400 {
				wantIssues = 0
			}
			if issues != wantIssues {
				t.Errorf("issues = %d, want %d", issues, wantIssues)
			}
		})
	}
}

func TestWebsocketTelemetryDuplexBoundaries(t *testing.T) {
	capture := captureWebsocketTelemetry(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
	telemetry := newResponsesWebsocketTelemetry(c, c.Request.Context(), "test-model")
	_, attempt := observability.BeginAttempt(telemetry.ctx, "codex", "test-model")
	attempt.Finish(nil, http.StatusOK)
	for _, payload := range []string{
		`{"type":"response.created","response":{"id":"r1"}}`,
		`{"type":"response.completed","response":{"id":"r1"}}`,
		`{"type":"response.done","response":{"id":"r1"}}`,
		`{"type":"response.created","response":{"id":"r2"}}`,
		`{"type":"response.failed","status":400,"response":{"id":"queued-rejection"}}`,
		`{"type":"response.output_text.delta","delta":"secret-first-token"}`,
		`{"type":"response.output_text.delta","delta":"secret-next-token"}`,
		`{"type":"response.completed","response":{"id":"r2"}}`,
	} {
		telemetry.beforePayload(c, []byte(payload))
		telemetry.afterPayload(c, []byte(payload))
	}
	telemetry.closed(c, nil)
	telemetry.finish(c, nil, context.Canceled)
	events := capture()
	assertWebsocketTelemetryOutcomes(t, events, []string{"success", "success"})
	timings := 0
	for _, event := range events {
		if strings.Contains(event.Raw, "secret-") || strings.Contains(event.Raw, "queued-rejection") {
			t.Error("payload data leaked into telemetry")
		}
		if event.Get("transaction").Exists() {
			if event.Get("tags.provider").String() != "codex" || event.Get("tags.requested_model").String() != "test-model" {
				t.Error("successor lost trusted provider/model dimensions")
			}
		}
		for _, metric := range event.Get("items").Array() {
			if metric.Get("name").String() == "llm.request.ttft" {
				timings++
			}
		}
	}
	if timings != 1 {
		t.Errorf("successor TTFT observations = %d, want 1", timings)
	}
}

func TestWebsocketTelemetryBufferedFailureBeforeDataClose(t *testing.T) {
	capture := captureWebsocketTelemetry(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
	telemetry := newResponsesWebsocketTelemetry(c, c.Request.Context(), "test-model")
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: errors.New("upstream unavailable")}
	close(errs)
	telemetry.closed(c, errs)
	telemetry.finish(c, nil, websocket.ErrCloseSent)
	assertWebsocketTelemetryOutcomes(t, capture(), []string{"upstream_5xx"})
}
