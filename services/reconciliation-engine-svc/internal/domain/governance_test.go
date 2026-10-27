package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestException_EdgesAreClosed(t *testing.T) {
	ok := [][2]ExceptionStatus{
		{ExceptionOpen, ExceptionInvestigating}, {ExceptionOpen, ExceptionAcceptedRisk}, {ExceptionOpen, ExceptionClosed},
		{ExceptionInvestigating, ExceptionRemediated}, {ExceptionInvestigating, ExceptionAcceptedRisk},
		{ExceptionRemediated, ExceptionInvestigating}, {ExceptionRemediated, ExceptionClosed},
	}
	for _, c := range ok {
		if err := ValidateExceptionTransition(c[0], c[1]); err != nil {
			t.Errorf("%s -> %s: %v", c[0], c[1], err)
		}
	}
	bad := [][2]ExceptionStatus{
		{ExceptionOpen, ExceptionRemediated}, // remediation is investigated first
		{ExceptionRemediated, ExceptionAcceptedRisk},
		{ExceptionAcceptedRisk, ExceptionOpen}, {ExceptionAcceptedRisk, ExceptionClosed}, // acceptance is final
		{ExceptionClosed, ExceptionOpen},
	}
	for _, c := range bad {
		if err := ValidateExceptionTransition(c[0], c[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s: %v, want ErrInvalidTransition", c[0], c[1], err)
		}
	}
	if !ExceptionAcceptedRisk.Unresolved() || ExceptionAcceptedRisk.Working() || ExceptionClosed.Unresolved() || !ExceptionRemediated.Working() {
		t.Fatal("accepted risk is unresolved but not worked; remediated is still worked; closed is resolved")
	}
}

func TestMateriality_DerivesSeverity(t *testing.T) {
	p := MaterialityPolicy{Currencies: map[string]int64{"USD": 1000}}
	for _, c := range []struct {
		ccy      string
		exposure int64
		sev      Severity
		class    MaterialityClass
	}{
		{"USD", 1000, SeverityBlocker, Material},
		{"USD", 999, SeverityError, Immaterial},
		{"USD", 100, SeverityError, Immaterial},
		{"USD", 99, SeverityWarning, Immaterial},
		{"USD", 0, SeverityWarning, Immaterial},
		{"EUR", 1, SeverityError, Unassessed}, // no threshold: never assumed immaterial
	} {
		sev, class := p.DeriveSeverity(c.ccy, c.exposure)
		if sev != c.sev || class != c.class {
			t.Errorf("%s %d: %s/%s, want %s/%s", c.ccy, c.exposure, sev, class, c.sev, c.class)
		}
	}
	if err := (MaterialityPolicy{Currencies: map[string]int64{"USD": 0}}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero threshold: %v", err)
	}
	if err := (MaterialityPolicy{Currencies: map[string]int64{"usd": 5}}).Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad currency: %v", err)
	}
	if ExposureOf(-5) != 5 || ExposureOf(-9223372036854775808) != 9223372036854775807 {
		t.Fatal("exposure is |v|, saturating")
	}
}

func TestExceptionPolicy_Validate(t *testing.T) {
	good := ExceptionPolicy{OwnerRole: "recon-owner", SLADays: map[Severity]int{SeverityBlocker: 1, SeverityError: 3, SeverityWarning: 10}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	due := good.DueAt(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), SeverityError)
	if !due.Equal(time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("due %v", due)
	}
	for name, p := range map[string]ExceptionPolicy{
		"no owner":        {SLADays: good.SLADays},
		"missing sla":     {OwnerRole: "o", SLADays: map[Severity]int{SeverityBlocker: 1}},
		"zero sla":        {OwnerRole: "recon-owner", SLADays: map[Severity]int{SeverityBlocker: 0, SeverityError: 3, SeverityWarning: 10}},
		"unknown sev":     {OwnerRole: "recon-owner", SLADays: map[Severity]int{SeverityBlocker: 1, SeverityError: 3, SeverityWarning: 10, "FATAL": 1}},
		"info has no sla": {OwnerRole: "recon-owner", SLADays: map[Severity]int{SeverityBlocker: 1, SeverityError: 3, SeverityWarning: 10, SeverityInfo: 1}},
	} {
		if err := p.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTransitionRequest_Validate(t *testing.T) {
	for name, tc := range map[string]struct {
		r  TransitionExceptionRequest
		ok bool
	}{
		"investigate":                {TransitionExceptionRequest{ToState: ExceptionInvestigating, Reason: "looking"}, true},
		"closing is not a command":   {TransitionExceptionRequest{ToState: ExceptionClosed, Reason: "done"}, false},
		"no reason":                  {TransitionExceptionRequest{ToState: ExceptionInvestigating}, false},
		"remediation needs evidence": {TransitionExceptionRequest{ToState: ExceptionRemediated, Reason: "fixed"}, false},
		"remediated":                 {TransitionExceptionRequest{ToState: ExceptionRemediated, Reason: "fixed", EvidenceRef: "jrnl-1"}, true},
		"waiver needs authority":     {TransitionExceptionRequest{ToState: ExceptionAcceptedRisk, Reason: "small"}, false},
		"accepted":                   {TransitionExceptionRequest{ToState: ExceptionAcceptedRisk, Reason: "small", AuthorityRef: "CFO memo 12"}, true},
		"bad root cause":             {TransitionExceptionRequest{ToState: ExceptionInvestigating, Reason: "x", RootCauseCode: "bad code"}, false},
	} {
		err := tc.r.Validate()
		if tc.ok != (err == nil) {
			t.Errorf("%s: %v", name, err)
		}
	}
	acc := TransitionExceptionRequest{ToState: ExceptionAcceptedRisk, Reason: "r", AuthorityRef: "a"}
	if err := acc.CheckRootCause(SeverityBlocker); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a BLOCKER needs a root cause: %v", err)
	}
	if err := acc.CheckRootCause(SeverityError); err != nil {
		t.Fatalf("an ERROR does not: %v", err)
	}
}

func TestCheckIndependence(t *testing.T) {
	owner := "olga"
	if err := CheckIndependence(ExceptionInvestigating, "olga", &owner, "bot"); err != nil {
		t.Fatal(err)
	}
	if err := CheckIndependence(ExceptionInvestigating, "mallory", &owner, "bot"); !errors.Is(err, ErrNotExceptionOwner) {
		t.Fatalf("non-owner works it: %v", err)
	}
	if err := CheckIndependence(ExceptionRemediated, "olga", nil, "bot"); !errors.Is(err, ErrNotExceptionOwner) {
		t.Fatalf("unassigned exception worked: %v", err)
	}
	if err := CheckIndependence(ExceptionAcceptedRisk, "olga", &owner, "bot"); !errors.Is(err, ErrNotIndependent) {
		t.Fatalf("owner excuses own exception: %v", err)
	}
	if err := CheckIndependence(ExceptionAcceptedRisk, "bot", &owner, "bot"); !errors.Is(err, ErrNotIndependent) {
		t.Fatalf("raiser excuses own exception: %v", err)
	}
	if err := CheckIndependence(ExceptionAcceptedRisk, "carol", &owner, "bot"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCertifierIndependence(t *testing.T) {
	preparers := []string{"alice", "bob"}
	if err := CheckCertifierIndependence("alice", preparers); !errors.Is(err, ErrNotIndependentCertifier) {
		t.Fatalf("preparer certifies own run: %v", err)
	}
	if err := CheckCertifierIndependence("bob", preparers); !errors.Is(err, ErrNotIndependentCertifier) {
		t.Fatalf("an earlier preparer, not the last, certifies: %v", err)
	}
	if err := CheckCertifierIndependence("carol", preparers); err != nil {
		t.Fatal(err)
	}
}

// B34: every principal recorded as certifying the run is barred from revoking
// it, not only the last.
func TestCheckRevokerIndependence(t *testing.T) {
	certifiers := []string{"carol", "dave"}
	if err := CheckRevokerIndependence("carol", certifiers); !errors.Is(err, ErrRevokerIsCertifier) {
		t.Fatalf("certifier revokes own certification: %v", err)
	}
	if err := CheckRevokerIndependence("dave", certifiers); !errors.Is(err, ErrRevokerIsCertifier) {
		t.Fatalf("an earlier certifier, not the last, revokes: %v", err)
	}
	if err := CheckRevokerIndependence("erin", certifiers); err != nil {
		t.Fatal(err)
	}
}

// Scenario 25: individually immaterial exceptions become material in aggregate.
// A key-matched pair is counted once; a balance difference is not added on top
// of the items it is made of.
func TestAssessAggregate_Scenario25(t *testing.T) {
	p := MaterialityPolicy{Currencies: map[string]int64{"USD": 1000}}
	var exps []Exposure
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		exps = append(exps,
			Exposure{ExceptionID: "x" + id + "1", Status: ExceptionOpen, Currency: "USD", ItemID: id + "1", Counterpart: id + "2", Minor: 300},
			Exposure{ExceptionID: "x" + id + "2", Status: ExceptionOpen, Currency: "USD", ItemID: id + "2", Counterpart: id + "1", Minor: 300})
		_ = i
	}
	exps = append(exps, Exposure{ExceptionID: "bal", Kind: ExceptionKindBalance, Status: ExceptionOpen, Currency: "USD", Minor: 1500})
	got := AssessAggregate(exps, p)
	if len(got) != 1 || got[0].ExposureMinorUnits != "1500" || got[0].Assessment != Material || got[0].UnresolvedCount != 11 {
		t.Fatalf("aggregate: %+v", got)
	}
	// Each item alone is immaterial.
	if sev, class := p.DeriveSeverity("USD", 300); class != Immaterial || sev != SeverityError {
		t.Fatalf("individual: %s %s", sev, class)
	}
	// Closed exceptions do not count.
	for i := range exps {
		exps[i].Status = ExceptionClosed
	}
	if got := AssessAggregate(exps, p); len(got) != 0 {
		t.Fatalf("closed exceptions assessed: %+v", got)
	}
}

func TestAssess_CertificationGate(t *testing.T) {
	p := MaterialityPolicy{Currencies: map[string]int64{"USD": 1000}}
	permit := CertificationProfile{PermitAcceptedRisk: true}
	small := func(st ExceptionStatus) []Exposure {
		return []Exposure{{ExceptionID: "x1", Status: st, Currency: "USD", ItemID: "i1", Minor: 3},
			{ExceptionID: "x2", Kind: ExceptionKindBalance, Status: st, Currency: "USD", Minor: 3}}
	}
	if a := Assess(nil, p, CertificationProfile{}); a.ResultState != ResultMatch || !a.Certifiable {
		t.Fatalf("no exceptions: %+v", a)
	}
	if a := Assess(small(ExceptionOpen), p, permit); a.ResultState != ResultVariance || a.Certifiable {
		t.Fatalf("working exceptions certifiable: %+v", a)
	}
	a := Assess(small(ExceptionAcceptedRisk), p, CertificationProfile{})
	if a.ResultState != ResultVariance || a.Certifiable || !strings.Contains(strings.Join(a.CertificationBlocks, ";"), "profile") {
		t.Fatalf("accepted risk without a permitting profile: %+v", a)
	}
	a = Assess(small(ExceptionAcceptedRisk), p, permit)
	if a.ResultState != ResultVariance || !a.Certifiable || len(a.AcceptedRiskIDs) != 2 {
		t.Fatalf("immaterial accepted risk under a permitting profile must certify — as VARIANCE: %+v", a)
	}
	big := []Exposure{{ExceptionID: "x1", Status: ExceptionAcceptedRisk, Currency: "USD", ItemID: "i1", Minor: 1000}}
	if a := Assess(big, p, permit); a.Certifiable {
		t.Fatalf("material accepted risk certified: %+v", a)
	}
	eur := []Exposure{{ExceptionID: "x1", Status: ExceptionAcceptedRisk, Currency: "EUR", ItemID: "i1", Minor: 1}}
	if a := Assess(eur, p, permit); a.Certifiable {
		t.Fatalf("unassessable accepted risk certified: %+v", a)
	}

	// The lifecycle accepts a certified VARIANCE only when the assessment says so.
	if err := ValidateLifecycleTransition(RunRunning, RunCertified, ResultVariance, CertCertified, false); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("VARIANCE certified without permitted accepted risk: %v", err)
	}
	if err := ValidateLifecycleTransition(RunRunning, RunCertified, ResultVariance, CertCertified, true); err != nil {
		t.Fatalf("permitted accepted risk: %v", err)
	}
	if err := ValidateLifecycleTransition(RunRunning, RunCertified, ResultIndeterminate, CertCertified, true); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("INDETERMINATE certified: %v", err)
	}
}

func TestRuleClassification(t *testing.T) {
	for rule, want := range map[string][2]string{
		RuleUnmatched: {"MISSING", "COMPLETENESS"}, RuleDateTolerance: {"TIMING", "CUTOFF"},
		RuleAmountTolerance: {"MISMATCH", "ACCURACY"}, RuleMissingKey: {"DATA_QUALITY", "ACCURACY"},
	} {
		c, a := RuleClassification(rule)
		if c != want[0] || a != want[1] || !exceptionCategories[c] || !exceptionAssertions[a] {
			t.Errorf("%s: %s/%s", rule, c, a)
		}
	}
}
