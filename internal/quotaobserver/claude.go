package quotaobserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
	log "github.com/sirupsen/logrus"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// Manager exposes observation and authenticated HTTP only. There is deliberately
// no inference, selection, binding mutation, refresh, or quota-reset operation.
type Manager interface {
	GetByID(string) (*coreauth.Auth, bool)
	HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error)
	ObserveRoutingEligibility(string, string, time.Time) (bool, int)
	LookupSessionAffinity(string, string, string) (*coreauth.Auth, string)
}

type Fetcher interface {
	Fetch(context.Context, *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error)
}

type usageFetcher struct{ manager Manager }

func (f usageFetcher) Fetch(ctx context.Context, a *coreauth.Auth) (*quotaforecast.Window, *quotaforecast.Window, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if errRequest != nil {
		return nil, nil, errors.New("quota usage request could not be created")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	resp, errHTTP := f.manager.HttpRequest(ctx, a, req)
	if resp != nil && resp.Body != nil {
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Warn("failed to close quota usage response")
			}
		}()
	}
	if errHTTP != nil || resp == nil || resp.Body == nil {
		// Transport errors can contain credential-bearing proxy URLs. Never expose them.
		return nil, nil, errors.New("quota usage request failed")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, errors.New("quota usage endpoint rejected the request")
	}
	const maxBody = 64 << 10
	data, errRead := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if errRead != nil || len(data) > maxBody {
		return nil, nil, errors.New("quota usage response could not be read")
	}
	return parseUsage(data)
}

// parseUsage normalizes measured percentages to fractions. Only an explicit
// null reset paired with zero five-hour utilization establishes a reported idle
// window; missing fields and a null whole window remain unknown.
func parseUsage(data []byte) (*quotaforecast.Window, *quotaforecast.Window, error) {
	var body struct {
		FiveHour json.RawMessage `json:"five_hour"`
		Weekly   json.RawMessage `json:"seven_day"`
	}
	if errDecode := json.Unmarshal(data, &body); errDecode != nil {
		return nil, nil, errors.New("invalid quota usage response")
	}
	read := func(raw json.RawMessage, five bool) (*quotaforecast.Window, error) {
		if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
			return nil, nil
		}
		var measured struct {
			Utilization *float64        `json:"utilization"`
			Reset       json.RawMessage `json:"resets_at"`
		}
		if errDecode := json.Unmarshal(raw, &measured); errDecode != nil {
			return nil, errors.New("invalid quota usage window")
		}
		w := &quotaforecast.Window{}
		if measured.Utilization != nil {
			if *measured.Utilization < 0 || *measured.Utilization > 100 {
				return nil, errors.New("invalid quota usage utilization")
			}
			used := *measured.Utilization / 100
			w.Utilization = &used
		}
		if len(measured.Reset) > 0 && strings.TrimSpace(string(measured.Reset)) != "null" {
			var stamp string
			if errDecode := json.Unmarshal(measured.Reset, &stamp); errDecode != nil {
				return nil, errors.New("invalid quota reset timestamp")
			}
			reset, errParse := time.Parse(time.RFC3339Nano, stamp)
			if errParse != nil || reset.IsZero() {
				return nil, errors.New("invalid quota reset timestamp")
			}
			w.ResetAt = reset.UTC()
		} else if five && strings.TrimSpace(string(measured.Reset)) == "null" && w.Utilization != nil && *w.Utilization == 0 {
			w.Idle = true
		}
		return w, nil
	}
	five, errFive := read(body.FiveHour, true)
	if errFive != nil {
		return nil, nil, errFive
	}
	weekly, errWeekly := read(body.Weekly, false)
	if errWeekly != nil {
		return nil, nil, errWeekly
	}
	if five == nil && weekly == nil {
		return nil, nil, errors.New("quota usage response has no supported windows")
	}
	return five, weekly, nil
}
