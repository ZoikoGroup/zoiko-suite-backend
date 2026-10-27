package store

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/authorization-svc/internal/domain"
)

// B-01: a request over the maximum is refused, never clamped.
func TestSessionDuration_RefusesInsteadOfShortening(t *testing.T) {
	cases := []struct {
		requested, def, max, ceiling int
		want                         int
		refused                      bool
	}{
		{0, 3600, 28800, 86400, 3600, false},      // default
		{28800, 3600, 28800, 86400, 28800, false}, // exactly the maximum
		{28801, 3600, 28800, 86400, 0, true},      // one second over
		{90000, 3600, 0, 86400, 0, true},          // no configured max: the ceiling still refuses
		{0, 1800, 900, 7200, 900, false},          // a default above the max is capped (nobody asked for it)
	}
	for _, c := range cases {
		got, err := sessionDuration(c.requested, c.def, c.max, c.ceiling)
		if c.refused {
			if !errors.Is(err, domain.ErrSessionDurationExceeded) {
				t.Fatalf("%+v: want ErrSessionDurationExceeded, got %d %v", c, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%+v: got %d %v", c, got, err)
		}
	}
}

func TestStepUpColumns_RoundTrip(t *testing.T) {
	if a, at, s := stepUpColumns(nil); a != nil || at != nil || s != nil {
		t.Fatal("nil evidence must write NULLs")
	}
	if a, _, _ := stepUpColumns(&domain.StepUpEvidence{Assurance: "MFA"}); a != nil {
		t.Fatal("evidence without a time must write NULLs (the CHECK pairs them)")
	}
	in := &domain.StepUpEvidence{Assurance: "MFA", AuthnAt: time.Now().Truncate(time.Second), SessionContextID: "sc-1"}
	out := stepUpEvidence(stepUpColumns(in))
	if out == nil || out.Assurance != "MFA" || !out.AuthnAt.Equal(in.AuthnAt) || out.SessionContextID != "sc-1" {
		t.Fatalf("round trip: %+v", out)
	}
	if stepUpEvidence(nil, nil, nil) != nil {
		t.Fatal("a pre-000033 row has no evidence")
	}
}
