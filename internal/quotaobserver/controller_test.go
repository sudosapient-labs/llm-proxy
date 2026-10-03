package quotaobserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
)

var epoch = time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)

type testManager struct {
	mu                   sync.Mutex
	auths                map[string]*coreauth.Auth
	health               map[string]bool
	bound, bindingStatus string
	lookups              int
	http                 func(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error)
}

func newTestManager() *testManager {
	m := &testManager{auths: make(map[string]*coreauth.Auth), health: make(map[string]bool), bindingStatus: "bound", bound: "credential-A"}
	for _, alias := range []string{"A", "B", "C"} {
		id := "credential-" + alias
		m.auths[id] = &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-token", "email": "synthetic-private@example.test"}}
		m.health[id] = true
	}
	return m
}
func (m *testManager) GetByID(id string) (*coreauth.Auth, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return a.Clone(), true
}
func (m *testManager) HttpRequest(ctx context.Context, a *coreauth.Auth, r *http.Request) (*http.Response, error) {
	return m.http(ctx, a, r)
}
func (m *testManager) ObserveRoutingEligibility(id, model string, now time.Time) (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.health[id], 0
}
func (m *testManager) LookupSessionAffinity(provider, model, session string) (*coreauth.Auth, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lookups++
	if a := m.auths[m.bound]; a != nil {
		return a.Clone(), m.bindingStatus
	}
	return nil, m.bindingStatus
}

type testClock struct{ seconds atomic.Int64 }

func (c *testClock) Now() time.Time          { return epoch.Add(time.Duration(c.seconds.Load()) * time.Second) }
func (c *testClock) Advance(d time.Duration) { c.seconds.Add(int64(d / time.Second)) }

func testConfig() config.QuotaForecastConfig {
	c := config.DefaultQuotaForecastConfig()
	c.Enabled = true
	c.Model = "synthetic-claude-model"
	c.Accounts = []config.QuotaForecastAccount{{AuthID: "credential-A", Alias: "A"}, {AuthID: "credential-B", Alias: "B"}, {AuthID: "credential-C", Alias: "C"}}
	c.Assumptions.Demand = []quotaforecast.Demand{{Minutes: 360, UnitsPerHour: 30}}
	return c
}
func testWindow(used float64, reset time.Time) *quotaforecast.Window {
	return &quotaforecast.Window{Utilization: &used, ResetAt: reset}
}

type fetcherFunc func(context.Context, *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error)

func (f fetcherFunc) Fetch(ctx context.Context, a *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error) {
	return f(ctx, a)
}

func newController(t *testing.T, cfg config.QuotaForecastConfig, m *testManager, clock *testClock) (*Controller, chan Status) {
	t.Helper()
	c, err := New(cfg, m, filepath.Join(t.TempDir(), "private", "observations.jsonl"), clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan Status, 64)
	c.onReport = func(status Status) { reports <- status }
	t.Cleanup(c.Close)
	return c, reports
}
func awaitReport(t *testing.T, reports <-chan Status, predicate func(Status) bool) Status {
	t.Helper()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	for {
		select {
		case status := <-reports:
			if predicate(status) {
				return status
			}
		case <-guard.C:
			t.Fatal("collector did not publish expected report")
			return Status{}
		}
	}
}
func allObserved(s Status) bool {
	if len(s.Collection) != 3 {
		return false
	}
	for _, c := range s.Collection {
		if c.State != "observed" {
			return false
		}
	}
	return true
}

func TestLiveCollectorRecordsRatesAndKeepsSimulationLocal(t *testing.T) {
	m, clock := newTestManager(), &testClock{}
	c, reports := newController(t, testConfig(), m, clock)
	var calls atomic.Int64
	c.fetcher = fetcherFunc(func(ctx context.Context, a *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error) {
		calls.Add(1)
		used := .2 + float64(clock.seconds.Load())/36000
		return testWindow(used, epoch.Add(5*time.Hour)), testWindow(.4, epoch.Add(7*24*time.Hour)), nil
	})
	ticks := make(chan time.Time)
	c.Start(context.Background(), ticks)
	first := awaitReport(t, reports, allObserved)
	if first.Report.Forecasts[0].FiveHour.State != "insufficient_history" {
		t.Fatal("first sample fabricated a rate")
	}
	clock.Advance(time.Minute)
	ticks <- clock.Now()
	second := awaitReport(t, reports, func(s Status) bool {
		return allObserved(s) && s.Report.Measured.Accounts[0].ObservedAt.Equal(clock.Now()) && s.Report.Measured.Accounts[1].ObservedAt.Equal(clock.Now()) && s.Report.Measured.Accounts[2].ObservedAt.Equal(clock.Now())
	})
	if second.Report.Forecasts[0].FiveHour.BurnPerHour == nil {
		t.Fatal("live collection lost measured history")
	}
	before := calls.Load()
	sim, err := c.Snapshot("synthetic-private-session", "B")
	if err != nil {
		t.Fatal(err)
	}
	if sim.Report.Conversation.Account != "B" || sim.Report.Measured.Bound != "A" {
		t.Fatal("actual affinity or explicit pin was not used")
	}
	if calls.Load() != before || m.lookups != 1 {
		t.Fatal("simulation fetched usage instead of using cached measurements")
	}
	c.Close()
	data, errRead := os.ReadFile(c.journal.path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	for _, secret := range []string{"synthetic-token", "synthetic-private@example.test", "credential-A", "synthetic-private-session"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("journal retained private credential or session data: %s", secret)
		}
	}
	info, _ := os.Stat(c.journal.path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("journal is not private")
	}
	if _, ok := m.auths["credential-A"].Metadata["access_token"]; !ok {
		t.Fatal("collection modified credential metadata")
	}
}

func TestStalledAndFailedAccountsDoNotBlockFreshPoolReports(t *testing.T) {
	m, clock := newTestManager(), &testClock{}
	c, reports := newController(t, testConfig(), m, clock)
	started, cancelled := make(chan struct{}), make(chan struct{})
	var callsA atomic.Int64
	c.fetcher = fetcherFunc(func(ctx context.Context, a *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error) {
		switch a.ID {
		case "credential-A":
			callsA.Add(1)
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, nil, ctx.Err()
		case "credential-C":
			return nil, nil, errors.New("synthetic private failure")
		default:
			return testWindow(.2, epoch.Add(5*time.Hour)), testWindow(.4, epoch.Add(7*24*time.Hour)), nil
		}
	})
	ticks := make(chan time.Time)
	c.Start(context.Background(), ticks)
	<-started
	awaitReport(t, reports, func(s Status) bool {
		return len(s.Collection) == 3 && s.Collection[1].State == "observed" && s.Collection[2].State == "fetch_failed"
	})
	clock.Advance(16 * time.Minute)
	ticks <- clock.Now()
	stale := awaitReport(t, reports, func(s Status) bool {
		return s.Report.Measured.At.Equal(clock.Now()) && s.Report.Forecasts[1].FiveHour.State == "stale"
	})
	if callsA.Load() != 1 || stale.Collection[0].State != "in_flight" {
		t.Fatal("stalled account triggered overlapping calls")
	}
	c.Close()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel stalled fetch")
	}
}

func TestFailedFetchRetainsActualPassiveObservationTime(t *testing.T) {
	m, clock := newTestManager(), &testClock{}
	m.auths["credential-A"].Quota.ObservedAt = epoch
	m.auths["credential-A"].Quota.Signals = map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.2", "Anthropic-Ratelimit-Unified-5h-Reset": "2051240400", "Anthropic-Ratelimit-Unified-7d-Utilization": "0.4", "Anthropic-Ratelimit-Unified-7d-Reset": "2051827200"}
	c, reports := newController(t, testConfig(), m, clock)
	c.fetcher = fetcherFunc(func(context.Context, *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error) {
		return nil, nil, errors.New("failed")
	})
	ticks := make(chan time.Time)
	c.Start(context.Background(), ticks)
	awaitReport(t, reports, func(s Status) bool {
		return len(s.Collection) == 3 && s.Collection[0].State == "fetch_failed" && s.Collection[1].State == "fetch_failed" && s.Collection[2].State == "fetch_failed"
	})
	clock.Advance(16 * time.Minute)
	ticks <- clock.Now()
	s := awaitReport(t, reports, func(s Status) bool { return s.Report.Forecasts[0].FiveHour.State == "stale" })
	if !s.Report.Measured.Accounts[0].ObservedAt.Equal(epoch) || s.Collection[0].Source != "passive_headers" {
		t.Fatal("polling made passive data appear fresh")
	}
}

func TestSimulationPreservesBindingsOutsidePoolAndAbstainsOnAmbiguity(t *testing.T) {
	m, clock := newTestManager(), &testClock{}
	m.auths["outside"] = m.auths["credential-A"].Clone()
	m.auths["outside"].ID = "outside"
	m.health["outside"] = true
	m.bound = "outside"
	c, _ := newController(t, testConfig(), m, clock)
	for _, binding := range []string{"bound", "ambiguous", "unsupported"} {
		m.bindingStatus = binding
		s, err := c.Snapshot("private-session", "")
		if err != nil {
			t.Fatal(err)
		}
		if s.Report.Conversation.Account != "" || s.Report.Activation.Account != "" {
			t.Fatal("unresolved/outside binding received a replacement recommendation")
		}
	}
	encoded, err := json.Marshal(c.status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private-session") {
		t.Fatal("session identifier was stored")
	}
}

func TestSlowFetchRetainsRequestStartTimeAndClosedTickerCancels(t *testing.T) {
	m, clock := newTestManager(), &testClock{}
	cfg := testConfig()
	cfg.Accounts = cfg.Accounts[:1]
	c, reports := newController(t, cfg, m, clock)
	started, release := make(chan struct{}), make(chan struct{})
	c.fetcher = fetcherFunc(func(ctx context.Context, _ *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
		return testWindow(.2, epoch.Add(5*time.Hour)), testWindow(.4, epoch.Add(7*24*time.Hour)), nil
	})
	ticks := make(chan time.Time)
	c.Start(context.Background(), ticks)
	<-started
	clock.Advance(20 * time.Minute)
	close(release)
	s := awaitReport(t, reports, func(s Status) bool { return len(s.Collection) == 1 && s.Collection[0].State == "observed" })
	if !s.Report.Measured.Accounts[0].ObservedAt.Equal(epoch) || s.Report.Forecasts[0].FiveHour.State != "stale" {
		t.Fatal("slow response manufactured freshness")
	}
	close(ticks)
	c.Close()
}

func TestSimulationClockRollbackDoesNotBreakPeriodicObservation(t *testing.T) {
	m, clock := newTestManager(), &testClock{}
	c, reports := newController(t, testConfig(), m, clock)
	clock.Advance(time.Minute)
	if _, err := c.Snapshot("", ""); err != nil {
		t.Fatal(err)
	}
	clock.Advance(-time.Minute)
	c.mu.Lock()
	c.publishLocked(context.Background())
	c.mu.Unlock()
	s := awaitReport(t, reports, func(s Status) bool { return s.Report != nil })
	if !s.Report.Measured.At.Equal(epoch.Add(time.Minute)) || s.Error != "" {
		t.Fatal("backward wall clock poisoned observer after simulation")
	}
}
