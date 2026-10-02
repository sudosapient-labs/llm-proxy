// quota-forecast replays normalized local snapshots or periodically records a
// local snapshot file. It does not load proxy configuration or contact providers.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
	log "github.com/sirupsen/logrus"
)

type scenario struct {
	Assumptions quotaforecast.Assumptions `json:"assumptions"`
	Samples     []quotaforecast.Sample    `json:"samples"`
}

func main() {
	input := flag.String("input", "", "local scenario JSON (required)")
	watch := flag.String("watch", "", "local sample JSON to poll; omit to replay scenario samples")
	interval := flag.Duration("interval", time.Minute, "local polling interval, at least 1s")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if errRun := run(ctx, *input, *watch, *interval, os.Stdout, time.Now); errRun != nil {
		log.WithError(errRun).Error("quota forecast stopped")
		os.Exit(1)
	}
}

func readJSON(path string, dst any) error {
	f, errOpen := os.Open(path)
	if errOpen != nil {
		return fmt.Errorf("open local input: %w", errOpen)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.WithError(errClose).Warn("close local quota input")
		}
	}()
	// Reject oversized snapshots and unknown fields rather than silently accepting
	// credential files or ambiguous input. Decoder errors never include raw records.
	const maxSize = 4 << 20
	data, errRead := io.ReadAll(io.LimitReader(f, maxSize+1))
	if errRead != nil {
		return fmt.Errorf("read local input: %w", errRead)
	}
	if len(data) > maxSize {
		return fmt.Errorf("local input exceeds 4 MiB")
	}
	return decode(data, dst)
}

func run(ctx context.Context, input, watch string, interval time.Duration, out io.Writer, now func() time.Time) error {
	if input == "" {
		return fmt.Errorf("-input is required")
	}
	if interval < time.Second {
		return fmt.Errorf("-interval must be at least 1s")
	}
	var config scenario
	if errRead := readJSON(input, &config); errRead != nil {
		return errRead
	}
	observer, errNew := quotaforecast.New(config.Assumptions)
	if errNew != nil {
		return errNew
	}
	encoder := json.NewEncoder(out)
	emit := func(s quotaforecast.Sample) error {
		report, errObserve := observer.Observe(s)
		if errObserve != nil {
			return errObserve
		}
		if errEncode := encoder.Encode(report); errEncode != nil {
			return fmt.Errorf("write quota journal: %w", errEncode)
		}
		return nil
	}
	// Replay samples can seed history on observer restart.
	for _, s := range config.Samples {
		if errEmit := emit(s); errEmit != nil {
			return errEmit
		}
	}
	if watch == "" {
		if len(config.Samples) == 0 {
			return fmt.Errorf("replay requires samples")
		}
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		var s quotaforecast.Sample
		if errRead := readJSON(watch, &s); errRead != nil {
			return errRead
		}
		s.At = now().UTC()
		if errEmit := emit(s); errEmit != nil {
			return errEmit
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func decode(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(dst); errDecode != nil {
		return fmt.Errorf("invalid quota JSON: check schema, field types and timestamps")
	}
	if errDecode := decoder.Decode(new(any)); errDecode != io.EOF {
		return fmt.Errorf("expected exactly one quota JSON document")
	}
	return nil
}
