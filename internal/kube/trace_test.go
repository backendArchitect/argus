package kube

import "testing"

// TestPlausibleTypo guards the rule that keeps the near-miss list useful.
//
// The first version matched on a shared label KEY, which over-matched so badly on a live cluster
// that it named six unrelated workloads and pointed at bad-rollout when the answer was gapped.
// Both halves matter here: the misspellings must be caught, and the unrelated values must not be.
func TestPlausibleTypo(t *testing.T) {
	tests := []struct {
		have, want string
		plausible  bool
		why        string
	}{
		{"gapped", "gapped-api", true, "truncation: the real bug this was written for"},
		{"gapped-api", "gapped", true, "and the same pair the other way round"},
		{"checkout-apo", "checkout-api", true, "transposition — not a substring either way"},
		{"checkout-api", "checkout-apj", true, "single-character slip at the end"},
		{"web", "web-frontend", true, "short but a genuine prefix"},

		{"bad-rollout", "gapped-api", false, "unrelated workloads sharing the app key"},
		{"oom-victim", "checkout-api", false, "unrelated"},
		{"api", "web", false, "no shared prefix and neither contains the other"},
		{"prod", "production", true, "prefix of four is the threshold, and this is a real pair"},
		{"dev", "production", false, "three characters is below the threshold"},

		{"same", "same", false, "identical values are a match, not a near miss"},
		{"", "anything", false, "an empty label value is not a misspelling of anything"},
		{"anything", "", false, "nor is anything a misspelling of an empty selector value"},
	}
	for _, tc := range tests {
		if got := plausibleTypo(tc.have, tc.want); got != tc.plausible {
			t.Errorf("plausibleTypo(%q, %q) = %v, want %v — %s",
				tc.have, tc.want, got, tc.plausible, tc.why)
		}
	}
}
