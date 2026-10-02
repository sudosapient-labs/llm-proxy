package quotaforecast

import (
	"strconv"
	"strings"
	"time"
)

// ClaudeWindows normalizes only the two measured Claude utilization/reset pairs
// from QuotaState.Signals. Pass QuotaState.ObservedAt unchanged to Account.
// Header values use fractions, unlike usage-API percentages. Missing or malformed
// fields stay unknown. Headers cannot establish an idle first-use timer.
// Raw headers, account identifiers, and unrelated fields are never retained.
func ClaudeWindows(signals map[string]string) (fiveHour, weekly *Window) {
	// Reject conflicting case variants deterministically.
	values := make(map[string]string)
	conflicts := make(map[string]bool)
	for key, value := range signals {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if old, ok := values[key]; ok && old != value {
			conflicts[key] = true
		}
		values[key] = value
	}
	read := func(window string) *Window {
		prefix := "anthropic-ratelimit-unified-" + window + "-"
		w := &Window{}
		if !conflicts[prefix+"utilization"] {
			if used, errParse := strconv.ParseFloat(values[prefix+"utilization"], 64); errParse == nil && fraction(used) {
				w.Utilization = ptr(used)
			}
		}
		if !conflicts[prefix+"reset"] {
			if seconds, errParse := strconv.ParseInt(values[prefix+"reset"], 10, 64); errParse == nil && seconds > 0 && seconds <= 253402300799 {
				w.ResetAt = time.Unix(seconds, 0).UTC()
			}
		}
		if w.Utilization == nil && w.ResetAt.IsZero() {
			return nil
		}
		return w
	}
	return read("5h"), read("7d")
}
