package logging

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/observability"
)

// Exercise the real SDK envelope transport, existing recovery, and Gin's
// committed-200 behavior, rather than just invoking telemetry methods.
func TestSentryGinRecoveryStreamingAndPrivacy(t *testing.T) {
	var mu sync.Mutex
	var envelopes []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		envelopes = append(envelopes, string(body))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()
	dsn := "http://public@" + strings.TrimPrefix(sink.URL, "http://") + "/1"
	if err := observability.Start(config.SentryConfig{Enabled: true, DSN: dsn, TracesSampleRate: 1, QueueSize: 32, Models: []string{"test-model"}}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		observability.Shutdown(ctx)
	}()
	g := gin.New()
	g.Use(GinLogrusLogger(), SentryRequests(), GinLogrusRecovery())
	g.POST("/v1/panic", func(c *gin.Context) { panic("secret-panic-value") })
	g.POST("/v1/abort", func(c *gin.Context) { panic(http.ErrAbortHandler) })
	g.POST("/v1/stream", func(c *gin.Context) {
		if GetRequestID(c.Request.Context()) != "existing-id" {
			t.Error("request ID overwritten")
		}
		if c.GetHeader("sentry-trace") != "" || c.GetHeader("baggage") != "" {
			t.Error("telemetry headers were forwarded")
		}
		observability.ConfigureRequest(c.Request.Context(), "test-model", true)
		c.Status(200)
		c.Writer.WriteHeaderNow()
		observability.Outcome(c.Request.Context(), io.ErrUnexpectedEOF, 502)
	})
	for _, path := range []string{"/v1/panic", "/v1/stream"} {
		r := httptest.NewRequest("POST", path+"?token=secret-query", strings.NewReader("secret-prompt"))
		r = r.WithContext(WithRequestID(r.Context(), "existing-id"))
		r.Header.Set("Authorization", "Bearer secret-key")
		r.Header.Set("Cookie", "secret-cookie")
		r.Header.Set("baggage", "secret-baggage")
		r.Header.Set("sentry-trace", "0123456789abcdef0123456789abcdef-0123456789abcdef-1")
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		want := 500
		if path == "/v1/stream" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("response changed: %d", w.Code)
		}
	}
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("abort behavior changed: %v", got)
			}
		}()
		g.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/abort", nil))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !observability.Shutdown(ctx) {
		t.Fatal("flush failed")
	}
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(envelopes, "\n")
	if strings.Contains(all, "secret-") {
		t.Fatalf("sensitive telemetry: %s", all)
	}
	if strings.Count(all, `"type":"recovered_panic"`) != 1 {
		t.Fatalf("panic missing/duplicated: %s", all)
	}
	if !strings.Contains(all, `"outcome":"upstream_5xx"`) || !strings.Contains(all, `"http.response.status_code":200`) {
		t.Fatalf("post-200 failure lost: %s", all)
	}
	if !strings.Contains(all, `"outcome":"cancelled"`) {
		t.Fatal("aborted request was not classified as cancellation")
	}
}
