package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
)

func writeFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if errWrite := os.WriteFile(path, data, 0600); errWrite != nil {
		t.Fatal(errWrite)
	}
}
func TestReplayIsDeterministicAndLogsReasons(t *testing.T) {
	var first, second bytes.Buffer
	input := "../../docs/examples/quota-forecast.json"
	for _, out := range []*bytes.Buffer{&first, &second} {
		if err := run(context.Background(), input, "", time.Minute, out, func() time.Time { panic("replay must use sample clock") }); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("nondeterministic replay")
	}
	dec := json.NewDecoder(&first)
	count := 0
	for dec.More() {
		var r quotaforecast.Report
		if err := dec.Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.Conversation.Reason == "" || r.Activation.Reason == "" {
			t.Fatal("missing recommendation reasons")
		}
		if r.Assumptions.IndependentAccounts || r.Assumptions.FirstUseStartsTimer {
			t.Fatal("fixture must not assert unverified assumptions")
		}
		if count == 1 && r.Forecasts[0].FiveHour.BurnPerHour == nil {
			t.Fatal("replay lost history")
		}
		count++
	}
	if count != 2 {
		t.Fatalf("got %d reports", count)
	}
}

type cancelWriter struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.cancel()
	return n, err
}
func TestWatchUsesInjectedClockAndPreservesMeasurementTime(t *testing.T) {
	var cfg scenario
	if err := readJSON("../../docs/examples/quota-forecast.json", &cfg); err != nil {
		t.Fatal(err)
	}
	sample := cfg.Samples[0]
	cfg.Samples = nil
	dir := t.TempDir()
	input := filepath.Join(dir, "scenario.json")
	snapshot := filepath.Join(dir, "sample.json")
	writeFixture(t, input, cfg)
	writeFixture(t, snapshot, sample)
	now := sample.At.Add(31 * time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &cancelWriter{cancel: cancel}
	if err := run(ctx, input, snapshot, time.Hour, out, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	var r quotaforecast.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if !r.Measured.At.Equal(now) || !r.Measured.Accounts[0].ObservedAt.Equal(sample.Accounts[0].ObservedAt) || r.Forecasts[0].FiveHour.State != "stale" {
		t.Fatal("poll made old quota appear fresh")
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic disk failure") }
func TestInputAndJournalFailures(t *testing.T) {
	for _, data := range []string{`{"access_token":"sensitive"}`, `{} {}`, `{"samples":[{"at":"invalid"}]}`} {
		var cfg scenario
		if err := decode([]byte(data), &cfg); err == nil || bytes.Contains([]byte(err.Error()), []byte("sensitive")) {
			t.Fatal("invalid input accepted or raw input leaked")
		}
	}
	if err := run(context.Background(), "../../docs/examples/quota-forecast.json", "", time.Minute, brokenWriter{}, time.Now); err == nil {
		t.Fatal("journal failure ignored")
	}
}
