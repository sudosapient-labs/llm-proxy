package quotaforecast

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var epoch = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

func assumptions() Assumptions {
	return Assumptions{MaxAgeMinutes: 30, MinimumRunwayMinutes: 10, ActivationLeadMinutes: 30, WeeklyReserve: .05, WeeklyUnitsPerUnit: .1, ActivationCost: .1, IndependentAccounts: true, FirstUseStartsTimer: true, Demand: []Demand{{Minutes: 720, UnitsPerHour: 120}}}
}
func window(used float64, reset time.Time) *Window {
	return &Window{Utilization: ptr(used), ResetAt: reset}
}
func account(id string, used, week float64) Account {
	return Account{ID: id, ObservedAt: epoch, Healthy: true, FiveHour: window(used, epoch.Add(5*time.Hour)), Weekly: window(week, epoch.Add(7*24*time.Hour))}
}
func observer(t *testing.T, a Assumptions) *Observer {
	t.Helper()
	o, err := New(a)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func observe(t *testing.T, o *Observer, s Sample) Report {
	t.Helper()
	r, err := o.Observe(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func closeTo(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-7 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMeasuredRatesAndIndependentWeeklyDepletion(t *testing.T) {
	o := observer(t, assumptions())
	a := account("A", .2, .8)
	observe(t, o, Sample{At: epoch, Accounts: []Account{a}})
	a.ObservedAt = epoch.Add(10 * time.Minute)
	a.FiveHour = window(.3, a.FiveHour.ResetAt)
	a.Weekly = window(.9, a.Weekly.ResetAt)
	r := observe(t, o, Sample{At: a.ObservedAt.Add(5 * time.Minute), Accounts: []Account{a}})
	closeTo(t, *r.Forecasts[0].FiveHour.BurnPerHour, .6)
	closeTo(t, *r.Forecasts[0].FiveHour.DepletionMinutes, 65)
	closeTo(t, *r.Forecasts[0].Weekly.DepletionMinutes, 5)
	if r.Conversation.Account != "" {
		t.Fatal("weekly headroom must prevent a new conversation")
	}
}

func TestUnobservedAccountHasMissingForecastWithoutFabricatedTimestamp(t *testing.T) {
	o := observer(t, assumptions())
	r := observe(t, o, Sample{At: epoch, Accounts: []Account{{ID: "A", Healthy: true}}})
	if r.Forecasts[0].FiveHour.State != "missing" || r.Forecasts[0].Weekly.State != "missing" || r.Conversation.Account != "" {
		t.Fatal("unobserved quota fabricated availability")
	}
	if _, err := o.Observe(Sample{At: epoch, Accounts: []Account{{ID: "A", FiveHour: window(.2, epoch.Add(time.Hour))}}}); err == nil {
		t.Fatal("measured window accepted without observation timestamp")
	}
}

func TestForecastDiscontinuities(t *testing.T) {
	tests := []struct {
		name    string
		modify  func(*Account)
		elapsed time.Duration
		want    string
	}{
		{"zero burn", func(*Account) {}, 10 * time.Minute, "zero_burn"},
		{"reset changed", func(a *Account) { a.FiveHour.ResetAt = a.FiveHour.ResetAt.Add(time.Hour) }, 10 * time.Minute, "window_changed"},
		{"decrease", func(a *Account) { a.FiveHour.Utilization = ptr(.1) }, 10 * time.Minute, "window_changed"},
		{"missing", func(a *Account) { a.FiveHour = nil }, 10 * time.Minute, "missing"},
		{"missing utilization", func(a *Account) { a.FiveHour.Utilization = nil }, 10 * time.Minute, "missing"},
		{"missing reset", func(a *Account) { a.FiveHour.ResetAt = time.Time{} }, 10 * time.Minute, "missing_reset"},
		{"expired reset", func(*Account) {}, 5 * time.Hour, "reset_requires_observation"},
		{"long gap", func(*Account) {}, 31 * time.Minute, "history_gap"},
		{"exhausted", func(a *Account) { a.FiveHour.Utilization = ptr(1) }, 10 * time.Minute, "depleted"},
		{"reset first", func(a *Account) { a.FiveHour.Utilization = ptr(.201) }, 10 * time.Minute, "reset_before_depletion"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := observer(t, assumptions())
			a := account("A", .2, .3)
			observe(t, o, Sample{At: epoch, Accounts: []Account{a}})
			a.ObservedAt = epoch.Add(tt.elapsed)
			tt.modify(&a)
			r := observe(t, o, Sample{At: a.ObservedAt, Accounts: []Account{a}})
			if r.Forecasts[0].FiveHour.State != tt.want {
				t.Fatalf("got %+v, want %s", r.Forecasts[0].FiveHour, tt.want)
			}
			if tt.want == "zero_burn" && r.Forecasts[0].FiveHour.DepletionMinutes != nil {
				t.Fatal("zero burn has no finite depletion estimate")
			}
		})
	}
}

func TestRepeatedPollsDoNotRefreshObservationOrLoseRate(t *testing.T) {
	o := observer(t, assumptions())
	a := account("A", .2, .3)
	observe(t, o, Sample{At: epoch, Accounts: []Account{a}})
	a.ObservedAt = epoch.Add(10 * time.Minute)
	a.FiveHour.Utilization = ptr(.3)
	observe(t, o, Sample{At: a.ObservedAt, Accounts: []Account{a}})
	r := observe(t, o, Sample{At: epoch.Add(15 * time.Minute), Accounts: []Account{a}})
	closeTo(t, *r.Forecasts[0].FiveHour.BurnPerHour, .6)
	r = observe(t, o, Sample{At: epoch.Add(41 * time.Minute), Accounts: []Account{a}})
	if r.Forecasts[0].FiveHour.State != "stale" || r.Conversation.Account != "" || !strings.Contains(r.Activation.Reason, "stale") {
		t.Fatalf("stale snapshot reused: %+v", r)
	}
}

func TestRepeatedObservationComparesWindowValues(t *testing.T) {
	for _, weekly := range []bool{false, true} {
		for _, tt := range []struct {
			name    string
			change  func(*Window)
			wantErr bool
		}{
			{"equivalent timezone", func(w *Window) { w.ResetAt = w.ResetAt.In(time.FixedZone("offset", -5*60*60)) }, false},
			{"different reset", func(w *Window) { w.ResetAt = w.ResetAt.Add(time.Second) }, true},
			{"different utilization", func(w *Window) { w.Utilization = ptr(.4) }, true},
			{"missing utilization", func(w *Window) { w.Utilization = nil }, true},
		} {
			t.Run(fmt.Sprintf("weekly=%v/%s", weekly, tt.name), func(t *testing.T) {
				o := observer(t, assumptions())
				a := account("A", .2, .3)
				observe(t, o, Sample{At: epoch, Accounts: []Account{a}})
				a.ObservedAt = epoch.Add(time.Minute)
				a.FiveHour.Utilization = ptr(.21)
				a.Weekly.Utilization = ptr(.31)
				first := observe(t, o, Sample{At: a.ObservedAt, Accounts: []Account{a}})
				w := a.FiveHour
				if weekly {
					w = a.Weekly
				}
				tt.change(w)
				r, err := o.Observe(Sample{At: epoch.Add(2 * time.Minute), Accounts: []Account{a}})
				if (err != nil) != tt.wantErr {
					t.Fatalf("error = %v, want error = %v", err, tt.wantErr)
				}
				if err == nil {
					closeTo(t, *r.Forecasts[0].FiveHour.BurnPerHour, *first.Forecasts[0].FiveHour.BurnPerHour)
					closeTo(t, *r.Forecasts[0].Weekly.BurnPerHour, *first.Forecasts[0].Weekly.BurnPerHour)
					if !o.history["A"].previous.ObservedAt.Equal(epoch) {
						t.Fatal("equivalent repeated observation advanced history")
					}
				}
			})
		}
	}
}

func TestBindingPinAndPriority(t *testing.T) {
	a, b, c := account("A", .1, .1), account("B", .1, .1), account("C", .1, .1)
	b.Priority = 10
	tests := []struct {
		name, bound, pinned, want string
		unhealthy                 bool
	}{
		{"priority", "", "", "B", false},
		{"healthy binding", "A", "", "A", false},
		{"pin wins", "A", "C", "C", false},
		{"missing pin", "A", "D", "", false},
		{"failed binding", "A", "", "B", true},
		{"failed pin", "", "A", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aa := a
			aa.Healthy = !tt.unhealthy
			r := observe(t, observer(t, assumptions()), Sample{At: epoch, Accounts: []Account{c, b, aa}, Bound: tt.bound, Pinned: tt.pinned})
			if r.Conversation.Account != tt.want {
				t.Fatalf("got %+v, want %s", r.Conversation, tt.want)
			}
		})
	}
	// Forecast uncertainty alone is not a reason to migrate a healthy session.
	a.FiveHour = nil
	r := observe(t, observer(t, assumptions()), Sample{At: epoch, Accounts: []Account{a, b}, Bound: "A"})
	if r.Conversation.Account != "A" {
		t.Fatal("missing quota must not evict a healthy binding")
	}
	a.FiveHour = window(1, epoch.Add(time.Hour))
	r = observe(t, observer(t, assumptions()), Sample{At: epoch, Accounts: []Account{a, b}, Bound: "A"})
	if r.Conversation.Account != "B" {
		t.Fatal("known exhaustion must allow failover recommendation")
	}
}

func TestNewConversationUsesRunwayAndWeeklyReserve(t *testing.T) {
	a, b, c := account("A", .95, .1), account("B", .1, .96), account("C", .2, .4)
	a.Priority = 100
	b.Priority = 100
	r := observe(t, observer(t, assumptions()), Sample{At: epoch, Accounts: []Account{a, b, c}})
	if r.Conversation.Account != "C" {
		t.Fatalf("got %+v", r.Conversation)
	}
}

func TestInvalidObservationsAreAtomic(t *testing.T) {
	o := observer(t, assumptions())
	a := account("A", .2, .3)
	b := account("B", .2, .3)
	observe(t, o, Sample{At: epoch, Accounts: []Account{a, b}})
	a.ObservedAt = epoch.Add(time.Minute)
	b.FiveHour.Utilization = ptr(2)
	if _, err := o.Observe(Sample{At: a.ObservedAt, Accounts: []Account{a, b}}); err == nil {
		t.Fatal("invalid utilization accepted")
	}
	if !o.history["A"].latest.ObservedAt.Equal(epoch) {
		t.Fatal("partial observation committed")
	}
	for _, modify := range []func(*Account){
		func(a *Account) { a.ObservedAt = epoch.Add(-time.Minute) },
		func(a *Account) { a.ObservedAt = epoch.Add(time.Hour) },
		func(a *Account) { a.ObservedAt = epoch; a.FiveHour.Utilization = ptr(.4) },
		func(a *Account) { a.FiveHour.Utilization = ptr(math.NaN()) },
		func(a *Account) { a.FiveHour.Idle = true },
		func(a *Account) { a.ID = "secret@example.com" },
	} {
		bad := account("A", .2, .3)
		modify(&bad)
		if _, err := o.Observe(Sample{At: epoch.Add(time.Minute), Accounts: []Account{bad}}); err == nil {
			t.Fatal("invalid snapshot accepted")
		}
	}
}

func TestReportsAndInputsDoNotMutateHistory(t *testing.T) {
	o := observer(t, assumptions())
	a := account("A", .2, .3)
	r := observe(t, o, Sample{At: epoch, Accounts: []Account{a}})
	*a.FiveHour.Utilization = .99
	*r.Measured.Accounts[0].FiveHour.Utilization = .8
	r.Assumptions.Demand[0].UnitsPerHour = 999
	a = account("A", .3, .4)
	a.ObservedAt = epoch.Add(10 * time.Minute)
	r = observe(t, o, Sample{At: a.ObservedAt, Accounts: []Account{a}})
	closeTo(t, *r.Forecasts[0].FiveHour.BurnPerHour, .6)
	if r.Assumptions.Demand[0].UnitsPerHour != 120 {
		t.Fatal("report mutated assumptions")
	}
}

func TestDeterministicReplay(t *testing.T) {
	a, b, c := account("A", .8, .2), account("B", 0, .1), account("C", 0, .1)
	b.FiveHour = &Window{Utilization: ptr(0), Idle: true}
	c.FiveHour = copyWindow(b.FiveHour)
	s := Sample{At: epoch, Accounts: []Account{c, a, b}, Bound: "A"}
	first := observe(t, observer(t, assumptions()), s)
	s.Accounts = []Account{b, c, a}
	second := observe(t, observer(t, assumptions()), s)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("input ordering changed output")
	}
	if _, err := json.Marshal(first); err != nil {
		t.Fatal(err)
	}
}

func TestStaleExhaustionCannotEvictHealthyBinding(t *testing.T) {
	for _, expired := range []bool{false, true} {
		a := account("A", 1, .1)
		now := epoch.Add(31 * time.Minute)
		if expired {
			a.FiveHour.ResetAt = epoch.Add(10 * time.Minute)
			now = epoch.Add(11 * time.Minute)
		}
		r := observe(t, observer(t, assumptions()), Sample{At: now, Accounts: []Account{a, account("B", .1, .1)}, Bound: "A"})
		if r.Conversation.Account != "A" {
			t.Fatal("old quota exhaustion displaced a caller-reported healthy binding")
		}
	}
}

func TestInvalidAssumptions(t *testing.T) {
	for _, change := range []func(*Assumptions){
		func(a *Assumptions) { a.MaxAgeMinutes = 0 },
		func(a *Assumptions) { a.Demand = nil },
		func(a *Assumptions) { a.Demand = []Demand{{Minutes: 2880}, {Minutes: 1}} },
		func(a *Assumptions) { a.Demand[0].UnitsPerHour = math.Inf(1) },
		func(a *Assumptions) { a.WeeklyUnitsPerUnit = 0 },
		func(a *Assumptions) { a.WeeklyReserve = math.NaN() },
		func(a *Assumptions) { a.ActivationCost = -1 },
	} {
		a := assumptions()
		change(&a)
		if _, err := New(a); err == nil {
			t.Fatal("invalid assumptions accepted")
		}
	}
}

func TestTinyBurnRemainsJSONSerializable(t *testing.T) {
	o := observer(t, assumptions())
	a := account("A", 0, .3)
	observe(t, o, Sample{At: epoch, Accounts: []Account{a}})
	a.ObservedAt = epoch.Add(time.Minute)
	a.FiveHour.Utilization = ptr(math.SmallestNonzeroFloat64)
	r := observe(t, o, Sample{At: a.ObservedAt, Accounts: []Account{a}})
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	if r.Forecasts[0].FiveHour.State != "reset_before_depletion" {
		t.Fatal("tiny burn should not predict depletion in current window")
	}
}
