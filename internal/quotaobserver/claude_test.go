package quotaobserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type closeTrackedBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackedBody) Close() error { b.closed = true; return nil }

func TestFetcherClosesBodyEvenWhenTransportReturnsError(t *testing.T) {
	m := newTestManager()
	body := &closeTrackedBody{Reader: strings.NewReader("synthetic")}
	m.http = func(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
		return &http.Response{Body: body}, errors.New("synthetic transport error")
	}
	if _, _, err := (usageFetcher{m}).Fetch(context.Background(), m.auths["credential-A"]); err == nil || !body.closed {
		t.Fatal("error response body was not closed")
	}
}

func TestUsageParsingSeparatesIdleFromMissing(t *testing.T) {
	for _, tc := range []struct {
		name, body               string
		idle, missing, wantError bool
	}{
		{"explicit idle", `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":45,"resets_at":"2035-01-07T00:00:00Z"}}`, true, false, false},
		{"missing reset", `{"five_hour":{"utilization":0}}`, false, false, false},
		{"null window", `{"five_hour":null,"seven_day":{"utilization":0}}`, false, true, false},
		{"missing windows", `{"extra_usage":{"enabled":true}}`, false, true, true},
		{"invalid percent", `{"five_hour":{"utilization":101}}`, false, false, true},
		{"invalid reset", `{"five_hour":{"utilization":5,"resets_at":"bad"}}`, false, false, true},
		{"invalid body", `not JSON`, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			five, week, err := parseUsage([]byte(tc.body))
			if (err != nil) != tc.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil {
				return
			}
			if (five == nil) != tc.missing || (five != nil && five.Idle != tc.idle) {
				t.Fatalf("unexpected five-hour window: %+v", five)
			}
			if tc.name == "explicit idle" && (week == nil || *week.Utilization != .45 || week.ResetAt.IsZero()) {
				t.Fatal("weekly percentages were not normalized")
			}
		})
	}
}

func TestFetcherUsesOnlyReadOnlyUsageAndRedactsFailures(t *testing.T) {
	m := newTestManager()
	m.http = func(ctx context.Context, a *coreauth.Auth, r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != claudeUsageURL || r.Body != nil || r.Header.Get("Anthropic-Beta") != "oauth-2025-04-20" {
			t.Fatal("unexpected upstream operation")
		}
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("collector installed an upstream deadline")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"five_hour":{"utilization":25,"resets_at":"2035-01-01T05:00:00Z"},"seven_day":{"utilization":60,"resets_at":"2035-01-07T00:00:00Z"}}`))}, nil
	}
	five, week, err := (usageFetcher{m}).Fetch(context.Background(), m.auths["credential-A"])
	if err != nil || *five.Utilization != .25 || *week.Utilization != .6 {
		t.Fatalf("unexpected normalized result: %v", err)
	}
	m.http = func(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
		return nil, errors.New("https://user:synthetic-secret@proxy.example")
	}
	_, _, err = (usageFetcher{m}).Fetch(context.Background(), m.auths["credential-A"])
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("raw transport details leaked")
	}
	m.http = func(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("synthetic-secret"))}, nil
	}
	_, _, err = (usageFetcher{m}).Fetch(context.Background(), m.auths["credential-A"])
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal("raw provider response leaked")
	}
}
