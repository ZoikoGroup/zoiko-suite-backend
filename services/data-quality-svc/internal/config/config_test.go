package config

import (
	"testing"
	"time"
)

// The freshness window is read from DQ_EVIDENCE_MAX_AGE_SECONDS. Anything that
// is not a positive integer yields 0, which the store reads as "use the
// default" — an unlimited window is deliberately not expressible.
func TestEvidenceMaxAge(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      time.Duration
	}{
		{"unset", "", 0},
		{"one hour", "3600", time.Hour},
		{"zero does not disable", "0", 0},
		{"negative does not disable", "-5", 0},
		{"garbage does not disable", "forever", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DQ_EVIDENCE_MAX_AGE_SECONDS", tc.env)
			if got := Load().EvidenceMaxAge; got != tc.want {
				t.Fatalf("EvidenceMaxAge = %v, want %v", got, tc.want)
			}
		})
	}
}
