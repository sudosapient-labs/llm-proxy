package config

// SentryConfig uses explicit provider/model allowlists to bound dimensions and
// prevent client-controlled names from entering telemetry. Changes require restart.
type SentryConfig struct {
	Enabled          bool     `yaml:"enabled"`
	DSN              string   `yaml:"dsn"`
	Environment      string   `yaml:"environment"`
	Release          string   `yaml:"release"`
	Instance         string   `yaml:"instance"`
	TracesSampleRate float64  `yaml:"traces-sample-rate"`
	QueueSize        int      `yaml:"queue-size"`
	Providers        []string `yaml:"providers"`
	Models           []string `yaml:"models"`
}
