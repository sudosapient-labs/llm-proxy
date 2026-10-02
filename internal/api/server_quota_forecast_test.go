package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaobserver"
)

func TestQuotaForecastRoutesRequireManagementAuthAndUseCachedSource(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "synthetic-management-key")
	calls := 0
	server := newTestServerWithOptions(t, WithQuotaForecastSource(func(session, pin string) (quotaobserver.Status, error) {
		calls++
		if calls == 2 && (session != "private-session" || pin != "B") {
			t.Fatal("simulation context lost")
		}
		return quotaobserver.Status{Enabled: true, Mode: "observe-and-simulate"}, nil
	}))
	const endpoint = "/v8/management/observability/quota-forecast"
	request := func(method, path, body string, authorized bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if authorized {
			r.Header.Set("Authorization", "Bearer synthetic-management-key")
		}
		w := httptest.NewRecorder()
		server.engine.ServeHTTP(w, r)
		return w
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := endpoint
		if method == http.MethodPost {
			path += "/simulate"
		}
		if w := request(method, path, `{}`, false); w.Code != http.StatusUnauthorized {
			t.Fatalf("unprotected quota endpoint: %d", w.Code)
		}
	}
	if calls != 0 {
		t.Fatal("unauthenticated requests invoked observer")
	}
	if w := request(http.MethodGet, endpoint, "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("GET failed: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPost, endpoint+"/simulate", `{"session_id":"private-session","pinned":"B"}`, true); w.Code != 200 || strings.Contains(w.Body.String(), "private-session") {
		t.Fatalf("simulation failed or leaked session: %d", w.Code)
	}
	if w := request(http.MethodPost, endpoint+"/simulate", `{"session_id":"`+strings.Repeat("x", 9000)+`"}`, true); w.Code != 400 {
		t.Fatal("oversized request accepted")
	}
	if calls != 2 {
		t.Fatal("invalid request invoked observer")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		path := "/v0/management/observability/quota-forecast"
		if method == http.MethodPost {
			path += "/simulate"
		}
		if w := request(method, path, `{}`, true); w.Code != 404 {
			t.Fatal("deprecated v0 quota route added")
		}
	}
	server.mgmt.SetQuotaForecastSource(nil)
	if w := request(http.MethodGet, endpoint, "", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatal("disabled status unavailable")
	}
	server.mgmt.SetQuotaForecastSource(func(string, string) (quotaobserver.Status, error) {
		return quotaobserver.Status{}, errors.New("synthetic-private-details")
	})
	if w := request(http.MethodGet, endpoint, "", true); w.Code != 503 || strings.Contains(w.Body.String(), "synthetic-private-details") {
		t.Fatal("source failure was not redacted")
	}
}
