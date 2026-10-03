package auth

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// ObserveRoutingEligibility reads the same model support, availability and
// priority rules used by selection. It never selects an auth, advances a cursor,
// refreshes a token, or creates/renews a session binding.
func (m *Manager) ObserveRoutingEligibility(authID, model string, now time.Time) (healthy bool, priority int) {
	if m == nil {
		return false, 0
	}
	a, ok := m.GetByID(authID)
	if !ok || a == nil {
		return false, 0
	}
	priority = authPriority(a)
	if !m.authSupportsRouteModel(registry.GetGlobalRegistry(), a, model) {
		return false, priority
	}
	available, errAvailable := m.availableAuthsForRouteModelAcrossPriorities([]*Auth{a}, a.Provider, model, now)
	return errAvailable == nil && len(available) == 1, priority
}
