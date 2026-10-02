package cliproxy

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaobserver"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

func (s *Service) startQuotaForecast(ctx context.Context) {
	s.quotaForecastMu.Lock()
	s.quotaForecastCtx = ctx
	s.quotaForecastMu.Unlock()
	s.applyQuotaForecastConfig(s.cfg)
}

func (s *Service) applyQuotaForecastConfig(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	s.quotaForecastMu.Lock()
	defer s.quotaForecastMu.Unlock()
	if s.quotaForecastCtx == nil {
		return
	}
	settings := cfg.QuotaForecast
	settings.Accounts = slices.Clone(settings.Accounts)
	settings.Assumptions.Demand = slices.Clone(settings.Assumptions.Demand)
	if settings.JournalPath == "" {
		settings.JournalPath = filepath.Join(cfg.AuthDir, "quota-forecast", "observations.jsonl")
	} else if !filepath.IsAbs(settings.JournalPath) {
		settings.JournalPath = filepath.Join(filepath.Dir(s.configPath), settings.JournalPath)
	}
	supported := !cfg.Home.Enabled && s.coreManager != nil
	if s.quotaForecastConfig != nil && reflect.DeepEqual(*s.quotaForecastConfig, settings) &&
		(!settings.Enabled || (supported && s.quotaForecast != nil)) {
		return
	}
	if s.quotaForecast != nil {
		s.quotaForecast.Close()
		s.quotaForecast = nil
	}
	s.quotaForecastConfig = &settings
	s.quotaForecastError = ""
	if !settings.Enabled {
		return
	}
	if !supported {
		s.quotaForecastError = "quota forecasting is available only for locally managed credentials"
		log.Warn(s.quotaForecastError)
		return
	}
	worker, errNew := quotaobserver.New(settings, s.coreManager, settings.JournalPath, time.Now)
	if errNew != nil {
		s.quotaForecastError = "quota forecast collector could not be initialized"
		log.Warn(s.quotaForecastError)
		return
	}
	s.quotaForecast = worker
	worker.Start(s.quotaForecastCtx, nil)
	log.Info("integrated Claude quota observation and simulation started")
}

func (s *Service) stopQuotaForecast() {
	s.quotaForecastMu.Lock()
	defer s.quotaForecastMu.Unlock()
	s.quotaForecastCtx = nil
	if s.quotaForecast != nil {
		s.quotaForecast.Close()
		s.quotaForecast = nil
	}
	s.quotaForecastConfig = nil
}

func (s *Service) quotaForecastSnapshot(sessionID, pinned string) (quotaobserver.Status, error) {
	s.quotaForecastMu.Lock()
	worker, errMessage := s.quotaForecast, s.quotaForecastError
	s.quotaForecastMu.Unlock()
	if worker == nil {
		return quotaobserver.Status{Enabled: false, Mode: "observe-and-simulate", Error: errMessage}, nil
	}
	return worker.Snapshot(sessionID, pinned)
}
