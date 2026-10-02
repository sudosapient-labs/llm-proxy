// Package quotaforecast provides local, observation-only quota forecasts and
// counterfactual simulations. It has no provider or routing dependencies.
package quotaforecast

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"time"
)

// Window uses fractions in [0,1]. Idle must be explicit: a missing reset is not
// evidence that a first-use timer can be started. Nil utilization means missing.
type Window struct {
	Utilization *float64  `json:"utilization"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	Idle        bool      `json:"idle,omitempty"`
}

type Account struct {
	ID         string    `json:"id"`
	ObservedAt time.Time `json:"observed_at"`
	FiveHour   *Window   `json:"five_hour"`
	Weekly     *Window   `json:"weekly"`
	Healthy    bool      `json:"healthy"`
	Priority   int       `json:"priority,omitempty"`
}

// Sample is a complete pool snapshot. Health, priority, binding and pin are
// supplied by the caller for the model under consideration; no session IDs are stored.
type Sample struct {
	At       time.Time `json:"at"`
	Accounts []Account `json:"accounts"`
	Bound    string    `json:"bound,omitempty"`
	Pinned   string    `json:"pinned,omitempty"`
}

type Demand struct {
	Minutes      int     `json:"minutes"`
	UnitsPerHour float64 `json:"units_per_hour"`
}

// Assumptions are scenario inputs, never inferred from token counts. One unit
// consumes one percentage point of a five-hour budget on any account.
type Assumptions struct {
	MaxAgeMinutes         int      `json:"max_age_minutes"`
	MinimumRunwayMinutes  int      `json:"minimum_runway_minutes"`
	ActivationLeadMinutes int      `json:"activation_lead_minutes"`
	WeeklyReserve         float64  `json:"weekly_reserve"`
	WeeklyUnitsPerUnit    float64  `json:"weekly_units_per_unit"`
	ActivationCost        float64  `json:"activation_cost"`
	IndependentAccounts   bool     `json:"independent_accounts"`
	FirstUseStartsTimer   bool     `json:"first_use_starts_timer"`
	Demand                []Demand `json:"demand"`
}

type Forecast struct {
	State            string   `json:"state"`
	BurnPerHour      *float64 `json:"burn_fraction_per_hour,omitempty"`
	DepletionMinutes *float64 `json:"depletion_minutes,omitempty"`
}

type AccountForecast struct {
	ID       string   `json:"id"`
	FiveHour Forecast `json:"five_hour"`
	Weekly   Forecast `json:"weekly"`
}

type Recommendation struct {
	Account string `json:"account,omitempty"`
	Reason  string `json:"reason"`
}

type Comparison struct {
	Account    string  `json:"account"`
	WaitUnmet  float64 `json:"wait_unmet_units"`
	EarlyUnmet float64 `json:"early_unmet_units"`
}

type Report struct {
	Measured     Sample            `json:"measured"`
	Assumptions  Assumptions       `json:"assumptions"`
	Forecasts    []AccountForecast `json:"forecasts"`
	Conversation Recommendation    `json:"conversation"`
	Activation   Recommendation    `json:"activation"`
	Comparisons  []Comparison      `json:"comparisons,omitempty"`
}

type history struct{ previous, latest Account }

// Observer retains only two distinct observations per account. Sample.At is the
// controllable clock; repeated polls do not manufacture fresh measurements.
// Use from a single goroutine. Reports own their data and can be journaled as JSONL.
type Observer struct {
	assumptions Assumptions
	history     map[string]history
	last        time.Time
}

func New(a Assumptions) (*Observer, error) {
	if a.MaxAgeMinutes <= 0 || a.MaxAgeMinutes > 1440 || a.MinimumRunwayMinutes < 0 || a.MinimumRunwayMinutes > 2880 || a.ActivationLeadMinutes < 0 || a.ActivationLeadMinutes > 2880 {
		return nil, fmt.Errorf("invalid freshness or runway minutes")
	}
	if !fraction(a.WeeklyReserve) || !finite(a.WeeklyUnitsPerUnit) || a.WeeklyUnitsPerUnit <= 0 || a.WeeklyUnitsPerUnit > 100 || !finite(a.ActivationCost) || a.ActivationCost < 0 || a.ActivationCost >= 100 {
		return nil, fmt.Errorf("invalid weekly conversion, reserve or activation cost")
	}
	total := 0
	for _, d := range a.Demand {
		if d.Minutes <= 0 || d.Minutes > 2880 || !finite(d.UnitsPerHour) || d.UnitsPerHour < 0 || d.UnitsPerHour > 1e6 {
			return nil, fmt.Errorf("invalid demand segment")
		}
		total += d.Minutes
		if total > 2880 {
			return nil, fmt.Errorf("simulation horizon exceeds 48 hours")
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("demand profile is required (zero demand is allowed)")
	}
	a.Demand = append([]Demand(nil), a.Demand...)
	return &Observer{assumptions: a, history: make(map[string]history)}, nil
}

var aliasPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

func finite(v float64) bool   { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func fraction(v float64) bool { return finite(v) && v >= 0 && v <= 1 }
func ptr(v float64) *float64  { return &v }
func validWindow(w *Window, weekly bool) bool {
	if w == nil {
		return true
	}
	if w.Utilization != nil && !fraction(*w.Utilization) {
		return false
	}
	return !w.Idle || (!weekly && w.ResetAt.IsZero() && w.Utilization != nil && *w.Utilization == 0)
}
func equalWindow(a, b *Window) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Idle != b.Idle || !a.ResetAt.Equal(b.ResetAt) {
		return false
	}
	if a.Utilization == nil || b.Utilization == nil {
		return a.Utilization == b.Utilization
	}
	return *a.Utilization == *b.Utilization
}
func copyWindow(w *Window) *Window {
	if w == nil {
		return nil
	}
	c := *w
	if w.Utilization != nil {
		c.Utilization = ptr(*w.Utilization)
	}
	return &c
}
func copyAccount(a Account) Account {
	a.FiveHour = copyWindow(a.FiveHour)
	a.Weekly = copyWindow(a.Weekly)
	return a
}

func (o *Observer) Observe(s Sample) (Report, error) {
	if s.At.IsZero() || s.At.Before(o.last) {
		return Report{}, fmt.Errorf("sample clock must be nonzero and monotonic")
	}
	if len(s.Accounts) == 0 || len(s.Accounts) > 3 {
		return Report{}, fmt.Errorf("provide one to three accounts")
	}
	seen := make(map[string]bool)
	for _, a := range s.Accounts {
		if !aliasPattern.MatchString(a.ID) || seen[a.ID] {
			return Report{}, fmt.Errorf("account aliases must be unique, short alphanumeric labels")
		}
		seen[a.ID] = true
		if a.ObservedAt.IsZero() || a.ObservedAt.After(s.At) {
			return Report{}, fmt.Errorf("observation time must be nonzero and not in the future")
		}
		if !validWindow(a.FiveHour, false) || !validWindow(a.Weekly, true) {
			return Report{}, fmt.Errorf("invalid window utilization or idle marker")
		}
		h := o.history[a.ID]
		if a.ObservedAt.Before(h.latest.ObservedAt) {
			return Report{}, fmt.Errorf("out-of-order observation")
		}
		if a.ObservedAt.Equal(h.latest.ObservedAt) && (!equalWindow(a.FiveHour, h.latest.FiveHour) || !equalWindow(a.Weekly, h.latest.Weekly)) {
			return Report{}, fmt.Errorf("conflicting observation at the same timestamp")
		}
	}
	if (s.Bound != "" && !aliasPattern.MatchString(s.Bound)) || (s.Pinned != "" && !aliasPattern.MatchString(s.Pinned)) {
		return Report{}, fmt.Errorf("invalid bound or pinned alias")
	}
	s.Accounts = append([]Account(nil), s.Accounts...)
	sort.Slice(s.Accounts, func(i, j int) bool { return s.Accounts[i].ID < s.Accounts[j].ID })
	for id := range o.history {
		if !seen[id] {
			delete(o.history, id)
		}
	}
	r := Report{Measured: s, Assumptions: o.assumptions}
	r.Assumptions.Demand = append([]Demand(nil), o.assumptions.Demand...)
	for i, a := range s.Accounts {
		a = copyAccount(a)
		s.Accounts[i] = a
		h := o.history[a.ID]
		if a.ObservedAt.After(h.latest.ObservedAt) {
			h.previous = h.latest
			h.latest = copyAccount(a)
			o.history[a.ID] = h
		}
		r.Forecasts = append(r.Forecasts, AccountForecast{ID: a.ID, FiveHour: o.forecast(a.FiveHour, h.previous.FiveHour, a.ObservedAt, h.previous.ObservedAt, s.At), Weekly: o.forecast(a.Weekly, h.previous.Weekly, a.ObservedAt, h.previous.ObservedAt, s.At)})
	}
	o.last = s.At
	r.Conversation = o.recommend(s, r.Forecasts)
	r.Activation, r.Comparisons = o.compare(s, r.Forecasts)
	return r, nil
}

func (o *Observer) forecast(w, p *Window, at, before, now time.Time) Forecast {
	if w == nil || w.Utilization == nil {
		return Forecast{State: "missing"}
	}
	age := time.Duration(o.assumptions.MaxAgeMinutes) * time.Minute
	if now.Sub(at) > age {
		return Forecast{State: "stale"}
	}
	if w.Idle {
		return Forecast{State: "idle_timer_unverified"}
	}
	if w.ResetAt.IsZero() {
		return Forecast{State: "missing_reset"}
	}
	if !w.ResetAt.After(now) {
		return Forecast{State: "reset_requires_observation"}
	}
	if *w.Utilization >= 1 {
		return Forecast{State: "depleted", DepletionMinutes: ptr(0)}
	}
	if p == nil || p.Utilization == nil || before.IsZero() {
		return Forecast{State: "insufficient_history"}
	}
	if !p.ResetAt.Equal(w.ResetAt) || *w.Utilization < *p.Utilization {
		return Forecast{State: "window_changed"}
	}
	if at.Sub(before) > age {
		return Forecast{State: "history_gap"}
	}
	rate := (*w.Utilization - *p.Utilization) / at.Sub(before).Hours()
	f := Forecast{State: "burning", BurnPerHour: ptr(rate)}
	if rate == 0 {
		f.State = "zero_burn"
		return f
	}
	minutes := math.Max(0, (1-*w.Utilization)/rate*60-now.Sub(at).Minutes())
	if !finite(minutes) {
		f.State = "reset_before_depletion"
		return f
	}
	f.DepletionMinutes = ptr(minutes)
	if minutes == 0 {
		f.State = "projected_depleted"
	} else if minutes >= w.ResetAt.Sub(now).Minutes() {
		f.State = "reset_before_depletion"
	}
	return f
}

// available is deliberately independent of forecast confidence for a healthy
// binding. Fresh exhaustion in a current window or a caller-reported failure
// still prevents reuse; stale exhaustion cannot override reported health.
func (o *Observer) available(a Account, now time.Time) bool {
	if !a.Healthy {
		return false
	}
	for _, w := range []*Window{a.FiveHour, a.Weekly} {
		if w != nil && w.Utilization != nil && *w.Utilization >= 1 &&
			now.Sub(a.ObservedAt) <= time.Duration(o.assumptions.MaxAgeMinutes)*time.Minute &&
			(w.ResetAt.IsZero() || w.ResetAt.After(now)) {
			return false
		}
	}
	return true
}

func (o *Observer) remaining(a Account, f AccountForecast, now time.Time) (float64, float64, bool) {
	for _, w := range []*Window{a.FiveHour, a.Weekly} {
		if w == nil || w.Utilization == nil || (!w.Idle && (w.ResetAt.IsZero() || !w.ResetAt.After(now))) {
			return 0, 0, false
		}
	}
	if now.Sub(a.ObservedAt) > time.Duration(o.assumptions.MaxAgeMinutes)*time.Minute {
		return 0, 0, false
	}
	remaining := func(w *Window, f Forecast) float64 {
		used := *w.Utilization
		if f.BurnPerHour != nil {
			used += *f.BurnPerHour * now.Sub(a.ObservedAt).Hours()
		}
		return math.Max(0, (1-used)*100)
	}
	return remaining(a.FiveHour, f.FiveHour), remaining(a.Weekly, f.Weekly), true
}

func (o *Observer) recommend(s Sample, forecasts []AccountForecast) Recommendation {
	if s.Pinned != "" {
		for _, a := range s.Accounts {
			if a.ID == s.Pinned && o.available(a, s.At) {
				return Recommendation{a.ID, "Preserve explicit pin; quota forecast does not override it."}
			}
		}
		return Recommendation{Reason: "Pinned account is unavailable or absent; do not recommend a different credential."}
	}
	for _, a := range s.Accounts {
		if a.ID == s.Bound && o.available(a, s.At) {
			return Recommendation{a.ID, "Preserve healthy session binding regardless of priority or forecast uncertainty."}
		}
	}
	peak := 0.0
	for _, d := range o.assumptions.Demand {
		peak = math.Max(peak, d.UnitsPerHour)
	}
	best := -1
	bestRunway := -1.0
	bestWeekly := 0.0
	for i, a := range s.Accounts {
		five, week, ok := o.remaining(a, forecasts[i], s.At)
		if !ok || !o.available(a, s.At) || five <= 0 || week <= o.assumptions.WeeklyReserve*100 {
			continue
		}
		fiveRate, weekRate := peak, peak*o.assumptions.WeeklyUnitsPerUnit
		if rate := forecasts[i].FiveHour.BurnPerHour; rate != nil {
			fiveRate = math.Max(fiveRate, *rate*100)
		}
		if rate := forecasts[i].Weekly.BurnPerHour; rate != nil {
			weekRate = math.Max(weekRate, *rate*100)
		}
		runway := math.Inf(1)
		if fiveRate > 0 {
			runway = five / fiveRate * 60
		}
		if weekRate > 0 {
			runway = math.Min(runway, (week-o.assumptions.WeeklyReserve*100)/weekRate*60)
		}
		if runway < float64(o.assumptions.MinimumRunwayMinutes) {
			continue
		}
		if best < 0 || a.Priority > s.Accounts[best].Priority || (a.Priority == s.Accounts[best].Priority && runway > bestRunway) {
			best = i
			bestRunway = runway
			bestWeekly = week
		}
	}
	if best < 0 {
		return Recommendation{Reason: "No fresh, healthy account has the required assumed runway and weekly reserve; abstain."}
	}
	runwayText := fmt.Sprintf("%.1f minutes", bestRunway)
	if math.IsInf(bestRunway, 1) {
		runwayText = "no depletion under zero measured/assumed burn"
	}
	return Recommendation{s.Accounts[best].ID, fmt.Sprintf("Fresh quota offers %s of runway (required %d minutes), with %.1f%% weekly headroom (reserve %.1f%%), under the stated demand assumption. Priority, then runway, then alias break ties.", runwayText, o.assumptions.MinimumRunwayMinutes, bestWeekly, o.assumptions.WeeklyReserve*100)}
}
