package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type quotaLifecycleExecutor struct {
	serviceTestPluginExecutor
	started, cancelled chan struct{}
}

func (*quotaLifecycleExecutor) Identifier() string { return "claude" }
func (e *quotaLifecycleExecutor) HttpRequest(ctx context.Context, _ *coreauth.Auth, r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.String() != "https://api.anthropic.com/api/oauth/usage" {
		return nil, errors.New("unexpected operation in synthetic lifecycle test")
	}
	e.started <- struct{}{}
	<-ctx.Done()
	e.cancelled <- struct{}{}
	return nil, ctx.Err()
}

func awaitLifecycleSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("quota worker lifecycle did not complete")
	}
}

func TestServiceQuotaForecastStartupReloadDisableAndShutdown(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	e := &quotaLifecycleExecutor{started: make(chan struct{}, 10), cancelled: make(chan struct{}, 10)}
	m.RegisterExecutor(e)
	if _, err := m.Register(context.Background(), &coreauth.Auth{ID: "synthetic-credential", Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-token"}}); err != nil {
		t.Fatal(err)
	}
	q := internalconfig.DefaultQuotaForecastConfig()
	q.Enabled = true
	q.Model = "synthetic-model"
	q.Accounts = []config.QuotaForecastAccount{{AuthID: "synthetic-credential", Alias: "A"}}
	q.Assumptions.Demand = []quotaforecast.Demand{{Minutes: 60, UnitsPerHour: 10}}
	cfg := &config.Config{AuthDir: t.TempDir(), QuotaForecast: q}
	s := &Service{cfg: cfg, coreManager: m, configPath: filepath.Join(t.TempDir(), "config.yaml")}
	t.Cleanup(s.stopQuotaForecast)
	s.startQuotaForecast(context.Background())
	awaitLifecycleSignal(t, e.started)
	first := s.quotaForecast
	if first == nil || s.quotaForecastConfig.JournalPath != filepath.Join(cfg.AuthDir, "quota-forecast", "observations.jsonl") {
		t.Fatal("normal service startup did not install worker in auth volume")
	}
	s.applyQuotaForecastConfig(cfg)
	if s.quotaForecast != first {
		t.Fatal("unchanged reload restarted worker")
	}
	invalid := *cfg
	invalid.QuotaForecast.Interval = "1s"
	if commit := s.commitConfigUpdate(&invalid); commit.sequence != 0 || s.cfg != cfg || s.quotaForecast != first {
		t.Fatal("invalid reload changed live collector/config")
	}
	// Keep the last applied settings independent of an SDK caller's slices.
	cfg.QuotaForecast.Assumptions.Demand[0].UnitsPerHour = 20
	s.applyQuotaForecastConfig(cfg)
	awaitLifecycleSignal(t, e.cancelled)
	awaitLifecycleSignal(t, e.started)
	if s.quotaForecast == first {
		t.Fatal("in-place scenario change was hidden by aliased settings")
	}
	next := *cfg
	next.QuotaForecast.Interval = "2m"
	s.applyQuotaForecastConfig(&next)
	awaitLifecycleSignal(t, e.cancelled)
	awaitLifecycleSignal(t, e.started)
	if s.quotaForecast == first {
		t.Fatal("changed reload did not replace worker")
	}
	next.QuotaForecast.Enabled = false
	s.applyQuotaForecastConfig(&next)
	awaitLifecycleSignal(t, e.cancelled)
	status, err := s.quotaForecastSnapshot("", "")
	if err != nil || status.Enabled || s.quotaForecast != nil {
		t.Fatal("disabled collector still active")
	}
	s.applyQuotaForecastConfig(cfg)
	awaitLifecycleSignal(t, e.started)
	home := *cfg
	home.Home.Enabled = true
	s.applyQuotaForecastConfig(&home)
	awaitLifecycleSignal(t, e.cancelled)
	status, err = s.quotaForecastSnapshot("", "")
	if err != nil || status.Enabled || status.Error == "" {
		t.Fatal("Home mode should explicitly report unsupported collection")
	}
	s.applyQuotaForecastConfig(cfg)
	awaitLifecycleSignal(t, e.started)
	s.stopQuotaForecast()
	awaitLifecycleSignal(t, e.cancelled)
	if s.quotaForecast != nil || s.quotaForecastCtx != nil {
		t.Fatal("shutdown retained worker")
	}
}
