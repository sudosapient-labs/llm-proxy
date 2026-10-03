// Package quotaobserver connects the deterministic forecaster to the proxy's
// credentials, passive signals and read-only Claude usage endpoint.
package quotaobserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/quotaforecast"
	log "github.com/sirupsen/logrus"
)

type CollectionStatus struct {
	Alias         string    `json:"alias"`
	State         string    `json:"state"`
	Source        string    `json:"source,omitempty"`
	ObservedAt    time.Time `json:"observed_at,omitempty"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
}

type Status struct {
	Enabled       bool                  `json:"enabled"`
	Mode          string                `json:"mode"`
	Model         string                `json:"model,omitempty"`
	Report        *quotaforecast.Report `json:"report,omitempty"`
	Collection    []CollectionStatus    `json:"collection,omitempty"`
	JournalError  string                `json:"journal_error,omitempty"`
	BindingStatus string                `json:"binding_status,omitempty"`
	Error         string                `json:"error,omitempty"`
}

type fetchResult struct {
	alias  string
	five   *quotaforecast.Window
	weekly *quotaforecast.Window
	at     time.Time
	err    error
}

// Controller keeps network acquisition off the HTTP/selection path. Each account
// has at most one in-flight fetch; a stalled connection cannot block other accounts
// or periodic stale-data reports. No connection/read/request deadlines are added.
type Controller struct {
	mu         sync.Mutex
	cfg        config.QuotaForecastConfig
	manager    Manager
	fetcher    Fetcher
	now        func() time.Time
	interval   time.Duration
	pool       string
	journal    journal
	observer   *quotaforecast.Observer
	measured   map[string]quotaforecast.Account
	collection map[string]CollectionStatus
	inFlight   map[string]bool
	status     Status
	lastAt     time.Time
	cancel     context.CancelFunc
	done       chan struct{}
	closed     bool
	onReport   func(Status)
}

func New(cfg config.QuotaForecastConfig, manager Manager, journalPath string, now func() time.Time) (*Controller, error) {
	if errValidate := cfg.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if !cfg.Enabled || manager == nil {
		return nil, errors.New("quota forecast requires enabled configuration and an auth manager")
	}
	if now == nil {
		now = time.Now
	}
	observer, errNew := quotaforecast.New(cfg.Assumptions)
	if errNew != nil {
		return nil, errNew
	}
	cfg.Accounts = append([]config.QuotaForecastAccount(nil), cfg.Accounts...)
	cfg.Assumptions.Demand = append([]quotaforecast.Demand(nil), cfg.Assumptions.Demand...)
	sort.Slice(cfg.Accounts, func(i, j int) bool { return cfg.Accounts[i].Alias < cfg.Accounts[j].Alias })
	// A one-way key prevents restored history from crossing changed credential
	// mappings or models, without journaling credential IDs or file names.
	identity, _ := json.Marshal(struct {
		Model    string
		Accounts []config.QuotaForecastAccount
	}{cfg.Model, cfg.Accounts})
	digest := sha256.Sum256(identity)
	interval, _ := time.ParseDuration(cfg.Interval)
	c := &Controller{
		cfg: cfg, manager: manager, fetcher: usageFetcher{manager}, now: now, interval: interval,
		pool: hex.EncodeToString(digest[:]), journal: journal{filepath.Clean(journalPath), int64(cfg.JournalSizeMB) << 20, cfg.JournalFiles},
		observer: observer, measured: make(map[string]quotaforecast.Account), collection: make(map[string]CollectionStatus), inFlight: make(map[string]bool),
		status: Status{Enabled: true, Mode: "observe-and-simulate", Model: cfg.Model}, done: make(chan struct{}),
	}
	c.lastAt = now().UTC()
	seeds, errRestore := c.journal.restore(c.pool, c.lastAt)
	if errRestore != nil {
		c.status.JournalError = "journal restore failed; collecting a new baseline"
	}
	for _, seed := range seeds {
		if _, errObserve := observer.Observe(seed); errObserve != nil {
			c.status.JournalError = "journal history invalid; collecting a new baseline"
			c.observer, _ = quotaforecast.New(cfg.Assumptions)
			c.measured = make(map[string]quotaforecast.Account)
			c.collection = make(map[string]CollectionStatus)
			break
		}
		for _, a := range seed.Accounts {
			c.measured[a.ID] = a
			c.collection[a.ID] = CollectionStatus{Alias: a.ID, State: "restored", Source: "journal", ObservedAt: a.ObservedAt}
		}
	}
	return c, nil
}

// Start accepts an optional tick channel for deterministic lifecycle tests.
// Production callers pass nil and use the configured real ticker.
func (c *Controller) Start(parent context.Context, ticks <-chan time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil || c.closed {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	go c.run(ctx, ticks)
}

func (c *Controller) Close() {
	c.mu.Lock()
	c.closed = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
		<-c.done
	}
}

func (c *Controller) run(ctx context.Context, ticks <-chan time.Time) {
	defer close(c.done)
	defer c.cancel()
	if ticks == nil {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	results := make(chan fetchResult, len(c.cfg.Accounts))
	c.cycle(ctx, results)
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			c.cycle(ctx, results)
		case result := <-results:
			if ctx.Err() != nil {
				return
			}
			c.mu.Lock()
			c.inFlight[result.alias] = false
			status := c.collection[result.alias]
			status.NextAttemptAt = result.at.Add(c.interval)
			if result.err != nil {
				status.State = "fetch_failed"
			} else {
				prior := c.measured[result.alias]
				if result.at.After(prior.ObservedAt) {
					c.measured[result.alias] = quotaforecast.Account{ID: result.alias, ObservedAt: result.at, FiveHour: result.five, Weekly: result.weekly}
					status.Source = "usage_api"
				}
				status.State = "observed"
			}
			c.collection[result.alias] = status
			c.publishLocked(ctx)
			c.mu.Unlock()
		}
	}
}

func (c *Controller) cycle(ctx context.Context, results chan<- fetchResult) {
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().UTC()
	for _, mapping := range c.cfg.Accounts {
		status := c.collection[mapping.Alias]
		status.Alias = mapping.Alias
		a, ok := c.manager.GetByID(mapping.AuthID)
		if !ok || a == nil || a.Provider != "claude" || a.AuthKind() != coreauth.AuthKindOAuth {
			status.State = "credential_unavailable"
			c.collection[mapping.Alias] = status
			continue
		}
		five, weekly := quotaforecast.ClaudeWindows(a.Quota.Signals)
		if !a.Quota.ObservedAt.IsZero() && !a.Quota.ObservedAt.After(now) && a.Quota.ObservedAt.After(c.measured[mapping.Alias].ObservedAt) && (five != nil || weekly != nil) {
			c.measured[mapping.Alias] = quotaforecast.Account{ID: mapping.Alias, ObservedAt: a.Quota.ObservedAt, FiveHour: five, Weekly: weekly}
			status.Source = "passive_headers"
		}
		token, tokenPresent := a.Metadata["access_token"].(string)
		if a.Disabled || a.Status == coreauth.StatusDisabled {
			status.State = "disabled"
		} else if expiry, hasExpiry := a.AccessTokenExpirationTime(); (hasExpiry && !expiry.After(now)) || !tokenPresent || strings.TrimSpace(token) == "" {
			status.State = "token_unavailable"
		} else if c.inFlight[mapping.Alias] {
			status.State = "in_flight"
		} else if !status.NextAttemptAt.After(now) {
			c.inFlight[mapping.Alias] = true
			status.State = "in_flight"
			status.LastAttemptAt = now
			go func(alias string, auth *coreauth.Auth, startedAt time.Time) {
				five, weekly, errFetch := c.fetcher.Fetch(ctx, auth)
				// A slow response cannot manufacture a fresh measurement at completion.
				result := fetchResult{alias, five, weekly, startedAt, errFetch}
				select {
				case results <- result:
				case <-ctx.Done():
				}
			}(mapping.Alias, a, now)
		}
		c.collection[mapping.Alias] = status
	}
	c.publishLocked(ctx)
}

func (c *Controller) sampleLocked() quotaforecast.Sample {
	sample := quotaforecast.Sample{At: c.now().UTC()}
	if sample.At.Before(c.lastAt) {
		sample.At = c.lastAt
	}
	c.lastAt = sample.At
	for _, mapping := range c.cfg.Accounts {
		a := c.measured[mapping.Alias]
		a.ID = mapping.Alias
		a.Healthy, a.Priority = c.manager.ObserveRoutingEligibility(mapping.AuthID, c.cfg.Model, sample.At)
		if auth, ok := c.manager.GetByID(mapping.AuthID); !ok || auth == nil || auth.Provider != "claude" || auth.AuthKind() != coreauth.AuthKindOAuth {
			a.Healthy = false
		}
		sample.Accounts = append(sample.Accounts, a)
	}
	return sample
}

func (c *Controller) publishLocked(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	report, errObserve := c.observer.Observe(c.sampleLocked())
	if errObserve != nil {
		c.status.Error = "quota observation could not be evaluated"
		return
	}
	c.status.Error = ""
	c.status.Report = &report
	c.status.Collection = nil
	for _, mapping := range c.cfg.Accounts {
		status := c.collection[mapping.Alias]
		status.Alias = mapping.Alias
		status.ObservedAt = c.measured[mapping.Alias].ObservedAt
		c.status.Collection = append(c.status.Collection, status)
	}
	if errAppend := c.journal.append(record{c.pool, c.cfg.Model, report, c.status.Collection}); errAppend != nil {
		c.status.JournalError = "quota journal write failed"
		log.Warn("quota forecast observation could not be persisted")
	} else {
		c.status.JournalError = ""
	}
	log.WithFields(log.Fields{"conversation_account": report.Conversation.Account, "conversation_reason": report.Conversation.Reason, "activation_account": report.Activation.Account, "activation_reason": report.Activation.Reason}).Info("quota forecast simulated recommendation")
	if c.onReport != nil {
		c.onReport(c.status)
	}
}

// Snapshot evaluates cached observations and actual affinity without network calls.
// Session IDs are used only for the read-only lookup and never stored or journaled.
func (c *Controller) Snapshot(sessionID, pinnedAlias string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return Status{Enabled: false, Mode: "observe-and-simulate"}, nil
	}
	status := c.status
	status.Collection = append([]CollectionStatus(nil), status.Collection...)
	sample := c.sampleLocked()
	sample.Pinned = pinnedAlias
	outsideHealthy := false
	if sessionID != "" {
		bound, bindingStatus := c.manager.LookupSessionAffinity("claude", c.cfg.Model, sessionID)
		status.BindingStatus = bindingStatus
		if bound != nil && bindingStatus == "bound" {
			for _, mapping := range c.cfg.Accounts {
				if mapping.AuthID == bound.ID {
					sample.Bound = mapping.Alias
				}
			}
			if sample.Bound == "" {
				outsideHealthy, _ = c.manager.ObserveRoutingEligibility(bound.ID, c.cfg.Model, sample.At)
			}
		}
	}
	report, errObserve := c.observer.Observe(sample)
	if errObserve != nil {
		return Status{}, errors.New("quota simulation unavailable")
	}
	if sessionID != "" && (status.BindingStatus == "ambiguous" || status.BindingStatus == "unsupported") {
		report.Conversation = quotaforecast.Recommendation{Reason: "Session affinity cannot be resolved safely; no replacement recommendation."}
		report.Activation = quotaforecast.Recommendation{Reason: "No early activation: session affinity cannot be resolved safely."}
		report.Comparisons = nil
	} else if outsideHealthy && pinnedAlias == "" {
		report.Conversation = quotaforecast.Recommendation{Reason: "Preserve the healthy session binding outside the configured pool; no replacement recommendation."}
		report.Activation = quotaforecast.Recommendation{Reason: "No early activation: the healthy session is bound outside the configured pool."}
		report.Comparisons = nil
	}
	status.Report = &report
	return status, nil
}
