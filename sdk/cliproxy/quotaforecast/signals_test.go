package quotaforecast

import "testing"

func TestClaudeSignalsNormalizeFractionsAndDoNotInventIdle(t *testing.T) {
	five, week := ClaudeWindows(map[string]string{
		"Anthropic-Ratelimit-Unified-5h-Utilization": "0.25",
		"Anthropic-Ratelimit-Unified-5h-Reset":       "1893474000",
		"anthropic-ratelimit-unified-7d-utilization": "0.7",
		"Authorization": "not-retained",
	})
	closeTo(t, *five.Utilization, .25)
	closeTo(t, *week.Utilization, .7)
	if five.ResetAt.Unix() != 1893474000 || week.Idle || !week.ResetAt.IsZero() {
		t.Fatal("incorrect reset normalization")
	}
	for _, v := range []string{"NaN", "Inf", "25", "-0.1", ""} {
		five, _ = ClaudeWindows(map[string]string{"anthropic-ratelimit-unified-5h-utilization": v})
		if five != nil {
			t.Fatalf("malformed signal accepted: %s", v)
		}
	}
	five, _ = ClaudeWindows(map[string]string{"Anthropic-Ratelimit-Unified-5h-Utilization": "0.1", "anthropic-ratelimit-unified-5h-utilization": "0.2"})
	if five != nil {
		t.Fatal("conflicting case variants accepted")
	}
}
