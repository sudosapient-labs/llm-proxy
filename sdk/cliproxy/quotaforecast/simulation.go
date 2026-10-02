package quotaforecast

import (
	"fmt"
	"math"
	"sort"
	"time"
)

const epsilon = 1e-8

type budget struct {
	id                   string
	priority             int
	five, week           float64
	fiveReset, weekReset time.Time
	idle                 bool
}

func (o *Observer) compare(s Sample, forecasts []AccountForecast) (Recommendation, []Comparison) {
	no := func(reason string) (Recommendation, []Comparison) { return Recommendation{Reason: reason}, nil }
	if s.Pinned != "" {
		return no("No early activation: an explicit pin restricts this scenario to one account.")
	}
	if !o.assumptions.IndependentAccounts || !o.assumptions.FirstUseStartsTimer {
		return no("No early activation: independent budgets and first-use timer activation must be explicit scenario assumptions, not inferred facts.")
	}
	var pool []budget
	near := false
	leadMinutes := o.assumptions.ActivationLeadMinutes
	demandWithinLead := 0.0
	for _, segment := range o.assumptions.Demand {
		minutes := min(leadMinutes, segment.Minutes)
		demandWithinLead += segment.UnitsPerHour * float64(minutes) / 60
		leadMinutes -= minutes
		if leadMinutes == 0 {
			break
		}
	}
	for i, a := range s.Accounts {
		if !a.Healthy {
			continue
		}
		five, week, ok := o.remaining(a, forecasts[i], s.At)
		if !ok {
			return no("No early activation: a healthy account has missing, stale, or expired quota; refresh observations.")
		}
		pool = append(pool, budget{a.ID, a.Priority, five, week, a.FiveHour.ResetAt, a.Weekly.ResetAt, a.FiveHour.Idle})
		if !a.FiveHour.Idle {
			leadHours := float64(o.assumptions.ActivationLeadMinutes) / 60
			fiveNeed, weekNeed := demandWithinLead, demandWithinLead*o.assumptions.WeeklyUnitsPerUnit
			if r := forecasts[i].FiveHour.BurnPerHour; r != nil {
				fiveNeed = math.Max(fiveNeed, *r*100*leadHours)
			}
			if r := forecasts[i].Weekly.BurnPerHour; r != nil {
				weekNeed = math.Max(weekNeed, *r*100*leadHours)
			}
			if five <= fiveNeed+epsilon || week <= weekNeed+epsilon {
				near = true
			}
		}
	}
	if !near {
		return no("No early activation: no active account is approaching depletion within the configured lead time.")
	}
	// Stable priority ordering is shared by both policies; an early activation
	// changes only the chosen idle account's timer and consumes the stated cost.
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].priority > pool[j].priority })
	wait := o.simulate(pool, s.At, s.Bound, "")
	best := wait
	result := Recommendation{Reason: "Wait: early activation does not reduce predicted unmet demand; ties favor no activation."}
	var comparisons []Comparison
	for _, b := range pool {
		if !b.idle || b.week <= o.assumptions.WeeklyReserve*100+o.assumptions.ActivationCost*o.assumptions.WeeklyUnitsPerUnit {
			continue
		}
		early := o.simulate(pool, s.At, s.Bound, b.id)
		comparisons = append(comparisons, Comparison{b.id, wait, early})
		if early < best-epsilon {
			best = early
			result = Recommendation{b.id, fmt.Sprintf("Simulate activation now: predicted unmet demand falls from %.3f to %.3f units over the supplied horizon, including activation cost. Conditional on the stated assumptions.", wait, early)}
		}
	}
	if len(comparisons) == 0 {
		result.Reason = "No early activation: no idle account has sufficient weekly headroom for activation cost and reserve."
	}
	return result, comparisons
}

// simulate drops unmet demand, uses one-minute steps, and rounds resets up to
// the next step boundary. This is a pooled workload model, not a request scheduler.
func (o *Observer) simulate(initial []budget, now time.Time, bound, early string) float64 {
	pool := append([]budget(nil), initial...)
	active := -1
	for i := range pool {
		if pool[i].id == bound {
			active = i
		}
		if pool[i].id == early {
			pool[i].idle = false
			pool[i].fiveReset = now.Add(5 * time.Hour)
			pool[i].five -= o.assumptions.ActivationCost
			pool[i].week -= o.assumptions.ActivationCost * o.assumptions.WeeklyUnitsPerUnit
		}
	}
	unmet := 0.0
	for _, segment := range o.assumptions.Demand {
		for minute := 0; minute < segment.Minutes; minute++ {
			for i := range pool {
				b := &pool[i]
				if !b.idle && !b.fiveReset.After(now) {
					b.five = 100
					b.idle = true
					b.fiveReset = time.Time{}
				}
				if !b.weekReset.After(now) {
					b.week = 100
					b.weekReset = b.weekReset.Add(7 * 24 * time.Hour)
				}
			}
			need := segment.UnitsPerHour / 60
			for need > epsilon {
				capacity := func(i int) float64 {
					return math.Max(0, math.Min(pool[i].five, pool[i].week/o.assumptions.WeeklyUnitsPerUnit))
				}
				if active < 0 || capacity(active) <= epsilon {
					active = -1
					for i := range pool {
						if capacity(i) > epsilon {
							active = i
							break
						}
					}
				}
				if active < 0 {
					break
				}
				b := &pool[active]
				if b.idle {
					b.idle = false
					b.fiveReset = now.Add(5 * time.Hour)
				}
				take := math.Min(need, capacity(active))
				b.five -= take
				b.week -= take * o.assumptions.WeeklyUnitsPerUnit
				need -= take
			}
			unmet += math.Max(0, need)
			now = now.Add(time.Minute)
		}
	}
	return unmet
}
