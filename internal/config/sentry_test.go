package config

import "testing"

func TestSentryV8Config(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nobservability:\n  sentry:\n    enabled: true\n    dsn: https://public@example.invalid/1\n    environment: staging\n    traces-sample-rate: 0.25\n    models: [test-model]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Sentry.Enabled || cfg.Sentry.Environment != "staging" || cfg.Sentry.TracesSampleRate != 0.25 || len(cfg.Sentry.Models) != 1 {
		t.Fatalf("sentry config lost: %+v", cfg.Sentry)
	}
}
