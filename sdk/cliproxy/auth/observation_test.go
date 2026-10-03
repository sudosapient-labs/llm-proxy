package auth

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestObserveRoutingEligibilityUsesAvailabilityWithoutSelection(t *testing.T) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	m := NewManager(nil, nil, nil)
	reg := registry.GetGlobalRegistry()
	const id, model = "synthetic-quota-observer", "synthetic-observer-model"
	reg.RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(id) })
	for _, tc := range []struct {
		name    string
		change  func(*Auth)
		model   string
		healthy bool
	}{
		{"healthy", func(*Auth) {}, model, true},
		{"unsupported model", func(*Auth) {}, "unregistered-model", false},
		{"disabled", func(a *Auth) { a.Disabled = true }, model, false},
		{"expired token", func(a *Auth) { a.Metadata["expired"] = now.Add(-time.Minute).Format(time.RFC3339) }, model, false},
		{"model cooldown", func(a *Auth) {
			a.ModelStates = map[string]*ModelState{model: {Unavailable: true, NextRetryAfter: now.Add(time.Hour), Quota: QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Hour)}}}
		}, model, false},
		{"credential quota", func(a *Auth) {
			a.Quota = QuotaState{Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour)}
		}, model, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "synthetic-token"}, Attributes: map[string]string{"priority": "7"}}
			tc.change(a)
			if _, err := m.Register(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			before, _ := m.GetByID(id)
			for i := 0; i < 2; i++ {
				health, priority := m.ObserveRoutingEligibility(id, tc.model, now)
				if health != tc.healthy || priority != 7 {
					t.Fatalf("health=%v priority=%d", health, priority)
				}
			}
			after, _ := m.GetByID(id)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("observation mutated auth state")
			}
		})
	}
}
