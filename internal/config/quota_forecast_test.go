package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const syntheticQuotaConfig = `config-version: 8
observability:
  quota-forecast:
    enabled: true
    model: synthetic-claude-model
    accounts:
      - {auth-id: synthetic-credential-A, alias: A}
      - {auth-id: synthetic-credential-B, alias: B}
      - {auth-id: synthetic-credential-C, alias: C}
    assumptions:
      demand: [{minutes: 360, units-per-hour: 30}]
`

func TestQuotaForecastV8DefaultsAndSaveReload(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte(syntheticQuotaConfig))
	if err != nil {
		t.Fatal(err)
	}
	q := cfg.QuotaForecast
	if !q.Enabled || len(q.Accounts) != 3 || q.Interval != "1m" || q.JournalFiles != 3 || q.JournalSizeMB != 10 || q.Assumptions.Demand[0].UnitsPerHour != 30 || q.Assumptions.IndependentAccounts || q.Assumptions.FirstUseStartsTimer {
		t.Fatalf("unsafe or missing defaults: %+v", q)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err = os.WriteFile(path, []byte(syntheticQuotaConfig), 0600); err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadConfig(path)
	if err != nil || !reflect.DeepEqual(q, reloaded.QuotaForecast) {
		t.Fatalf("save/reload changed forecast settings: %v", err)
	}
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), "quota-forecast:") || !strings.Contains(string(saved), "observability:") {
		t.Fatal("v8 section lost")
	}
	disabled, err := ParseConfigBytes([]byte("config-version: 8\n"))
	if err != nil || disabled.QuotaForecast.Enabled {
		t.Fatal("collector enabled without explicit opt-in")
	}
}

func TestQuotaForecastRejectsInvalidEnabledSettings(t *testing.T) {
	base, err := ParseConfigBytes([]byte(syntheticQuotaConfig))
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*QuotaForecastConfig){
		"poll interval":       func(q *QuotaForecastConfig) { q.Interval = "1s" },
		"model":               func(q *QuotaForecastConfig) { q.Model = "" },
		"missing credentials": func(q *QuotaForecastConfig) { q.Accounts = nil },
		"duplicate auth":      func(q *QuotaForecastConfig) { q.Accounts[1].AuthID = q.Accounts[0].AuthID },
		"duplicate alias":     func(q *QuotaForecastConfig) { q.Accounts[1].Alias = q.Accounts[0].Alias },
		"unsafe alias":        func(q *QuotaForecastConfig) { q.Accounts[0].Alias = "email@example.test" },
		"journal bounds":      func(q *QuotaForecastConfig) { q.JournalFiles = 0 },
		"journal extension":   func(q *QuotaForecastConfig) { q.JournalPath = "config.yaml" },
		"missing demand":      func(q *QuotaForecastConfig) { q.Assumptions.Demand = nil },
	} {
		t.Run(name, func(t *testing.T) {
			q := base.QuotaForecast
			q.Accounts = append([]QuotaForecastAccount(nil), q.Accounts...)
			change(&q)
			if q.Validate() == nil {
				t.Fatal("invalid enabled settings accepted")
			}
		})
	}
	if _, err = ParseConfigBytes([]byte(strings.Replace(syntheticQuotaConfig, "model: synthetic-claude-model", "interval: 1s\n    model: synthetic-claude-model", 1))); err == nil {
		t.Fatal("parser skipped quota validation")
	}
}
