package domain

import (
	"errors"
	"testing"
)

func TestResolveRequestValidation(t *testing.T) {
	ok := []ResolveExceptionRequest{
		{ToState: "INVESTIGATING", Reason: "looking"},
		{ToState: "REMEDIATED", Reason: "posted", EvidenceRef: "JE-1"},
		{ToState: "REPERFORMED", Reason: "rerun clean", EvidenceRef: "run-2"},
		{ToState: "WAIVED_UNDER_AUTHORITY", Reason: "immaterial", AuthorityRef: "CFO-2026-14"},
		{ToState: "CARRIED_FORWARD_UNDER_AUTHORITY", Reason: "timing", AuthorityRef: "CFO-2026-15", CarryToPeriod: "2026-10"},
		{ToState: "CLOSED", Reason: "done"},
	}
	for _, r := range ok {
		r := r
		if err := r.Validate(); err != nil {
			t.Errorf("%s: %v", r.ToState, err)
		}
	}
	bad := []ResolveExceptionRequest{
		{ToState: "ASSIGNED", Reason: "x"}, // the assign command owns this
		{ToState: "OPEN", Reason: "x"},
		{ToState: "INVESTIGATING"},                                                   // no reason
		{ToState: "REMEDIATED", Reason: "x"},                                         // no evidence
		{ToState: "REPERFORMED", Reason: "x"},                                        // no evidence
		{ToState: "WAIVED_UNDER_AUTHORITY", Reason: "x"},                             // no authority
		{ToState: "CARRIED_FORWARD_UNDER_AUTHORITY", Reason: "x", AuthorityRef: "a"}, // no period
		{ToState: "CARRIED_FORWARD_UNDER_AUTHORITY", Reason: "x", AuthorityRef: "a", CarryToPeriod: "2026-13"},
	}
	for _, r := range bad {
		r := r
		if err := r.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%+v: want invalid argument, got %v", r, err)
		}
	}
}

func TestRequiredActionSeparatesWaiverAndReperformance(t *testing.T) {
	cases := map[string]string{
		"INVESTIGATING":                   ActionResolve,
		"REMEDIATED":                      ActionResolve,
		"CLOSED":                          ActionResolve,
		"REPERFORMED":                     ActionReperfrm,
		"WAIVED_UNDER_AUTHORITY":          ActionWaive,
		"CARRIED_FORWARD_UNDER_AUTHORITY": ActionWaive,
	}
	for to, want := range cases {
		r := ResolveExceptionRequest{ToState: to}
		if got := r.RequiredAction(); got != want {
			t.Errorf("%s: got %s want %s", to, got, want)
		}
	}
}

func TestCheckIndependence(t *testing.T) {
	owner := "olivia"
	none := (*string)(nil)
	type c struct {
		to         ExceptionState
		actor      string
		owner      *string
		remediator string
		want       error
	}
	for i, tc := range []c{
		{ExInvestigating, "olivia", &owner, "", nil},
		{ExInvestigating, "mallory", &owner, "", ErrNotOwner},
		{ExAwaitingAdjustment, "olivia", none, "", ErrNotOwner}, // ownerless cannot be worked
		{ExRemediated, "olivia", &owner, "", nil},
		{ExWaived, "sam", &owner, "", nil},
		{ExWaived, "olivia", &owner, "", ErrNotIndependent}, // owner cannot excuse own exception
		{ExCarriedForward, "olivia", &owner, "", ErrNotIndependent},
		{ExReperformed, "rita", &owner, "olivia", nil},
		{ExReperformed, "olivia", &owner, "olivia", ErrNotIndependent},
		{ExReperformed, "remy", &owner, "remy", ErrNotIndependent}, // remediator cannot reperform
		{ExClosed, "anyone", &owner, "olivia", nil},
	} {
		if got := CheckIndependence(tc.to, tc.actor, tc.owner, tc.remediator); got != tc.want {
			t.Errorf("case %d (%s by %s): got %v want %v", i, tc.to, tc.actor, got, tc.want)
		}
	}
}

func TestRollupOutcome(t *testing.T) {
	if _, ok := RollupOutcome(1, 5); ok {
		t.Fatal("an unresolved exception blocks the roll-up")
	}
	if r, ok := RollupOutcome(0, 0); !ok || r != ResultPass {
		t.Fatalf("all fixed => PASS, got %v %v", r, ok)
	}
	if r, ok := RollupOutcome(0, 2); !ok || r != ResultPassWithApprovedEx {
		t.Fatalf("any waiver => PASS_WITH_APPROVED_EXCEPTIONS, got %v %v", r, ok)
	}
}

func TestRootCauseIsRequiredForHighSeverityOrRecurrent(t *testing.T) {
	mk := func(to string) ResolveExceptionRequest {
		r := ResolveExceptionRequest{ToState: to, Reason: "x", EvidenceRef: "e", AuthorityRef: "a", CarryToPeriod: "2026-10"}
		return r
	}
	for _, to := range []string{"REMEDIATED", "WAIVED_UNDER_AUTHORITY", "CARRIED_FORWARD_UNDER_AUTHORITY"} {
		r := mk(to)
		if err := r.CheckRootCause(SeverityHigh, false); err == nil {
			t.Errorf("%s: HIGH severity must need a root cause", to)
		}
		if err := r.CheckRootCause(SeverityLow, true); err == nil {
			t.Errorf("%s: a recurrent exception must need a root cause", to)
		}
		if err := r.CheckRootCause(SeverityLow, false); err != nil {
			t.Errorf("%s: a low, first-time exception needs none: %v", to, err)
		}
		r.RootCauseCode, r.RootCauseNote = "LATE_SUBLEDGER_FEED", "feed ran after cut-off"
		if err := r.CheckRootCause(SeverityHigh, true); err != nil {
			t.Errorf("%s: provided root cause must satisfy: %v", to, err)
		}
	}
	r := mk("CLOSED")
	if err := r.CheckRootCause(SeverityHigh, true); err != nil {
		t.Errorf("closing after reperformance does not need it again: %v", err)
	}
	bad := ResolveExceptionRequest{ToState: "REMEDIATED", Reason: "x", EvidenceRef: "e", RootCauseCode: "lower case"}
	if err := bad.Validate(); err == nil {
		t.Error("malformed root_cause_code must be rejected")
	}
}
