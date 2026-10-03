package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
)

// QuotaForecastAccount privately maps an existing credential to a public-safe alias.
type QuotaForecastAccount struct {
	AuthID string `yaml:"auth-id" json:"auth-id"`
	Alias  string `yaml:"alias" json:"alias"`
}

type QuotaForecastConfig struct {
	Enabled       bool                      `yaml:"enabled" json:"enabled"`
	Interval      string                    `yaml:"interval" json:"interval"`
	Model         string                    `yaml:"model" json:"model"`
	Accounts      []QuotaForecastAccount    `yaml:"accounts" json:"accounts"`
	JournalPath   string                    `yaml:"journal-path" json:"journal-path"`
	JournalSizeMB int                       `yaml:"journal-size-mb" json:"journal-size-mb"`
	JournalFiles  int                       `yaml:"journal-files" json:"journal-files"`
	Assumptions   quotaforecast.Assumptions `yaml:"assumptions" json:"assumptions"`
}

func DefaultQuotaForecastConfig() QuotaForecastConfig {
	return QuotaForecastConfig{
		Interval: "1m", JournalSizeMB: 10, JournalFiles: 3,
		Assumptions: quotaforecast.Assumptions{
			MaxAgeMinutes: 15, MinimumRunwayMinutes: 30, ActivationLeadMinutes: 30,
			WeeklyReserve: .1, WeeklyUnitsPerUnit: .1, ActivationCost: .1,
		},
	}
}

var quotaAliasPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

func (c QuotaForecastConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	interval, errParse := time.ParseDuration(c.Interval)
	if errParse != nil || interval < 30*time.Second || interval > time.Hour {
		return fmt.Errorf("observability.quota-forecast.interval must be between 30s and 1h")
	}
	if strings.TrimSpace(c.Model) == "" || len(c.Model) > 200 {
		return fmt.Errorf("observability.quota-forecast.model is required")
	}
	if len(c.Accounts) < 1 || len(c.Accounts) > 3 {
		return fmt.Errorf("observability.quota-forecast.accounts requires one to three credentials")
	}
	ids, aliases := make(map[string]bool), make(map[string]bool)
	for _, account := range c.Accounts {
		if strings.TrimSpace(account.AuthID) == "" || ids[account.AuthID] ||
			!quotaAliasPattern.MatchString(account.Alias) || aliases[account.Alias] {
			return fmt.Errorf("observability.quota-forecast.accounts requires unique auth IDs and short alphanumeric aliases")
		}
		ids[account.AuthID], aliases[account.Alias] = true, true
	}
	if c.JournalSizeMB < 1 || c.JournalSizeMB > 100 || c.JournalFiles < 1 || c.JournalFiles > 10 {
		return fmt.Errorf("observability.quota-forecast journal bounds must be 1–100 MiB and 1–10 files")
	}
	if c.JournalPath != "" && filepath.Ext(c.JournalPath) != ".jsonl" {
		return fmt.Errorf("observability.quota-forecast.journal-path must end in .jsonl")
	}
	if _, errNew := quotaforecast.New(c.Assumptions); errNew != nil {
		return fmt.Errorf("observability.quota-forecast.assumptions: %w", errNew)
	}
	return nil
}
