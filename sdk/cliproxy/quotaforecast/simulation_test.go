package quotaforecast

import (
	"strings"
	"testing"
	"time"
)

func idle(id string, week float64) Account {
	a := account(id, 0, week)
	a.FiveHour = &Window{Utilization: ptr(0), Idle: true}
	return a
}

func TestActivationTieAndWeeklyBottleneck(t *testing.T) {
	for _, weekly := range []float64{.1, .999} {
		a := assumptions()
		a.Demand = []Demand{{Minutes: 300, UnitsPerHour: 30}}
		r := observe(t, observer(t, a), Sample{At: epoch, Accounts: []Account{account("A", .9, .1), idle("B", weekly), idle("C", weekly)}, Bound: "A"})
		if r.Activation.Account != "" {
			t.Fatalf("earlier timer alone is not a benefit: %+v", r)
		}
		for _, c := range r.Comparisons {
			if c.EarlyUnmet < c.WaitUnmet-epsilon {
				t.Fatal("unexpected improvement")
			}
		}
	}
}

func TestActivationRequiresAssumptionsAndFreshness(t *testing.T) {
	for _, which := range []string{"independence", "timer", "stale", "missing", "pin"} {
		t.Run(which, func(t *testing.T) {
			a := assumptions()
			s := Sample{At: epoch, Accounts: []Account{account("A", .9, .1), idle("B", .1), idle("C", .1)}}
			switch which {
			case "independence":
				a.IndependentAccounts = false
			case "timer":
				a.FirstUseStartsTimer = false
			case "stale":
				s.At = epoch.Add(time.Hour)
			case "missing":
				s.Accounts[1].Weekly = nil
			case "pin":
				s.Pinned = "A"
			}
			r := observe(t, observer(t, a), s)
			if r.Activation.Account != "" || len(r.Comparisons) != 0 {
				t.Fatalf("unsafe comparison: %+v", r)
			}
		})
	}
}

func TestSimulationAccountsForWeeklyCapsAndResets(t *testing.T) {
	a := assumptions()
	a.WeeklyUnitsPerUnit = 1
	a.Demand = []Demand{{Minutes: 2, UnitsPerHour: 60}}
	o := observer(t, a)
	b := budget{id: "A", five: 100, week: 1, fiveReset: epoch.Add(5 * time.Hour), weekReset: epoch.Add(7 * 24 * time.Hour)}
	closeTo(t, o.simulate([]budget{b}, epoch, "A", ""), 1)
	b.weekReset = epoch.Add(time.Minute)
	closeTo(t, o.simulate([]budget{b}, epoch, "A", ""), 0)
	b.week = 100
	b.five = 0
	b.fiveReset = epoch.Add(30 * time.Second)
	closeTo(t, o.simulate([]budget{b}, epoch, "A", ""), 1) // reset rounds up, never grants capacity early
}

func TestSimulationDemandConservationAndActivationCost(t *testing.T) {
	a := assumptions()
	a.WeeklyUnitsPerUnit = 1
	a.Demand = []Demand{{Minutes: 60, UnitsPerHour: 100}}
	a.ActivationCost = 2
	o := observer(t, a)
	pool := []budget{{id: "A", five: 100, week: 100, idle: true, weekReset: epoch.Add(7 * 24 * time.Hour)}}
	closeTo(t, o.simulate(pool, epoch, "", ""), 0)
	closeTo(t, o.simulate(pool, epoch, "", "A"), 2)
	if pool[0].five != 100 || !pool[0].idle {
		t.Fatal("simulation modified initial state")
	}
	a.Demand = []Demand{{Minutes: 720, UnitsPerHour: 0}}
	closeTo(t, observer(t, a).simulate(pool, epoch, "", "A"), 0)
}

func TestEarlyActivationCanReduceUnmetDemand(t *testing.T) {
	a := assumptions()
	a.Demand = []Demand{{Minutes: 320, UnitsPerHour: 240}}
	// A runs out in five minutes and its replenished budget runs out again.
	// B's early timer covers five minutes of shortage near the horizon.
	active := account("A", .8, .1)
	active.FiveHour.ResetAt = epoch.Add(time.Hour)
	r := observe(t, observer(t, a), Sample{At: epoch, Accounts: []Account{active, idle("B", .1), idle("C", .1)}, Bound: "A"})
	if r.Activation.Account != "B" {
		t.Fatalf("expected useful early activation: %+v", r)
	}
	found := false
	for _, c := range r.Comparisons {
		if c.Account == r.Activation.Account {
			found = true
			if c.EarlyUnmet >= c.WaitUnmet {
				t.Fatal("activation lacks unmet-demand improvement")
			}
		}
	}
	if !found || !strings.Contains(r.Activation.Reason, "Conditional") {
		t.Fatal("missing evidence or assumption disclosure")
	}
}

func TestUpcomingDemandWithinLeadTriggersComparison(t *testing.T) {
	a := assumptions()
	a.Demand = []Demand{{Minutes: 1, UnitsPerHour: 0}, {Minutes: 319, UnitsPerHour: 240}}
	active := account("A", .8, .1)
	active.FiveHour.ResetAt = epoch.Add(time.Hour)
	r := observe(t, observer(t, a), Sample{At: epoch, Accounts: []Account{active, idle("B", .1), idle("C", .1)}, Bound: "A"})
	if len(r.Comparisons) != 2 {
		t.Fatal("quiet first minute hid depletion during the configured lead time")
	}
}
