package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestLifecycle_LegalAndIllegalEdges(t *testing.T) {
	legal := []struct{ from, to LifecycleState }{
		{LifecycleScheduled, LifecyclePreparing},
		{LifecyclePreparing, LifecyclePopulationFroze},
		{LifecyclePopulationFroze, LifecycleExecuting},
		{LifecycleExecuting, LifecycleExceptionReview},
		{LifecycleExceptionReview, LifecycleRemediation},
		{LifecycleRemediation, LifecycleReperformance},
		{LifecycleReperformance, LifecycleExecuting},
	}
	for _, c := range legal {
		if err := ValidateLifecycleTransition(c.from, c.to, ResultFail, CertPending); err != nil {
			t.Errorf("%s -> %s should be legal: %v", c.from, c.to, err)
		}
	}
	illegal := []struct{ from, to LifecycleState }{
		{LifecycleScheduled, LifecycleCertified},       // cannot skip execution
		{LifecycleExecuting, LifecycleCertified},       // cannot certify without readiness
		{LifecycleFailed, LifecycleExecuting},          // terminal
		{LifecycleSuperseded, LifecycleReadyToCertify}, // terminal
		{LifecycleCertified, LifecycleExceptionReview}, // certified runs are immutable
		{LifecycleExceptionReview, LifecycleCertified}, // must pass through READY_TO_CERTIFY
		{LifecyclePopulationFroze, LifecycleScheduled}, // no going back
	}
	for _, c := range illegal {
		err := ValidateLifecycleTransition(c.from, c.to, ResultPass, CertCertified)
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s should be refused, got %v", c.from, c.to, err)
		}
	}
}

// Invariants 5/9 + scenario 09: a run is never READY_TO_CERTIFY or CERTIFIED
// on a Fail / Indeterminate / Not Evaluated result.
func TestLifecycle_NonPassResultsAreNeverCertifiable(t *testing.T) {
	for _, res := range []ResultState{ResultFail, ResultIndeterminate, ResultNotEvaluated} {
		if err := ValidateLifecycleTransition(LifecycleExecuting, LifecycleReadyToCertify, res, CertPending); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("READY_TO_CERTIFY with result %s must be refused, got %v", res, err)
		}
		if err := ValidateLifecycleTransition(LifecycleReadyToCertify, LifecycleCertified, res, CertCertified); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("CERTIFIED with result %s must be refused, got %v", res, err)
		}
	}
	for _, res := range []ResultState{ResultPass, ResultPassWithApprovedEx} {
		if err := ValidateLifecycleTransition(LifecycleExecuting, LifecycleReadyToCertify, res, CertPending); err != nil {
			t.Errorf("READY_TO_CERTIFY with %s should be legal: %v", res, err)
		}
	}
}

func TestLifecycle_CertifiedRequiresCertifiedCertification(t *testing.T) {
	err := ValidateLifecycleTransition(LifecycleReadyToCertify, LifecycleCertified, ResultPass, CertPending)
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("CERTIFIED without certification_state CERTIFIED must be refused, got %v", err)
	}
}

func TestLifecycle_OnlyCertifiedCanBeSupersededFromCertified(t *testing.T) {
	if err := ValidateLifecycleTransition(LifecycleCertified, LifecycleSuperseded, ResultPass, CertCertified); err != nil {
		t.Fatalf("CERTIFIED -> SUPERSEDED must be legal: %v", err)
	}
	if !TerminalLifecycle(LifecycleFailed) || !TerminalLifecycle(LifecycleExpired) || !TerminalLifecycle(LifecycleSuperseded) {
		t.Fatal("FAILED, EXPIRED and SUPERSEDED are terminal")
	}
	if TerminalLifecycle(LifecycleCertified) {
		t.Fatal("CERTIFIED is not terminal: it may be superseded")
	}
}

func TestResultTransition_FrozenAndPreExecution(t *testing.T) {
	for _, lc := range []LifecycleState{LifecycleFailed, LifecycleCertified, LifecycleSuperseded, LifecycleExpired} {
		if err := ValidateResultTransition(lc, ResultFail, ResultPass); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("result must be frozen in %s, got %v", lc, err)
		}
	}
	for _, lc := range []LifecycleState{LifecycleScheduled, LifecyclePreparing, LifecyclePopulationFroze} {
		if err := ValidateResultTransition(lc, ResultNotEvaluated, ResultPass); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("no result may be recorded in %s, got %v", lc, err)
		}
	}
	if err := ValidateResultTransition(LifecycleExecuting, ResultNotEvaluated, ResultPass); err != nil {
		t.Errorf("recording a result during execution is legal: %v", err)
	}
	if err := ValidateResultTransition(LifecycleReperformance, ResultFail, ResultNotEvaluated); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a result cannot be reset to NOT_EVALUATED, got %v", err)
	}
}

func TestCertificationTransitions(t *testing.T) {
	ok := [][2]CertificationState{{CertNotRequired, CertPending}, {CertPending, CertCertified},
		{CertPending, CertRejected}, {CertRejected, CertPending}, {CertCertified, CertRevoked}, {CertCertified, CertSuperseded}}
	for _, c := range ok {
		if err := ValidateCertificationTransition(c[0], c[1]); err != nil {
			t.Errorf("%s -> %s should be legal: %v", c[0], c[1], err)
		}
	}
	bad := [][2]CertificationState{{CertNotRequired, CertCertified}, {CertRevoked, CertCertified},
		{CertSuperseded, CertPending}, {CertCertified, CertPending}}
	for _, c := range bad {
		if err := ValidateCertificationTransition(c[0], c[1]); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s should be refused, got %v", c[0], c[1], err)
		}
	}
}

// §21: an exception is not control-resolved until reperformed; there is no
// Remediated -> Closed shortcut, and waived/carried-forward are terminal.
func TestExceptionTransitions_NoCloseWithoutReperformance(t *testing.T) {
	if err := ValidateExceptionTransition(ExRemediated, ExClosed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("REMEDIATED -> CLOSED must be refused, got %v", err)
	}
	if err := ValidateExceptionTransition(ExOpen, ExClosed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("OPEN -> CLOSED must be refused (scenario: manual deletion to pass), got %v", err)
	}
	path := []ExceptionState{ExOpen, ExAssigned, ExInvestigating, ExAwaitingAdjustment, ExRemediated, ExReperformed, ExClosed}
	for i := 0; i+1 < len(path); i++ {
		if err := ValidateExceptionTransition(path[i], path[i+1]); err != nil {
			t.Errorf("%s -> %s should be legal: %v", path[i], path[i+1], err)
		}
	}
	for _, s := range []ExceptionState{ExWaived, ExCarriedForward, ExClosed} {
		if err := ValidateExceptionTransition(s, ExInvestigating); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s is terminal, got %v", s, err)
		}
	}
}

func validDef() CreateControlDefinitionRequest {
	return CreateControlDefinitionRequest{
		ControlCode: "FIN-CTRL-001", Name: "AR subledger to GL", Domain: "AR", ControlType: "BALANCE",
		Assertions: []string{"completeness", "accuracy"}, RiskTier: "KEY", Frequency: "PERIOD_END",
		CloseGating: true, OwnerRole: "AR_CONTROLLER", ReviewerRole: "SENIOR_ACCOUNTANT", CertifierRole: "CONTROLLER",
		SourceSpec:     json.RawMessage(`{"system":"accounts-receivable-svc"}`),
		TargetSpec:     json.RawMessage(`{"system":"general-ledger-svc"}`),
		EvidencePolicy: json.RawMessage(`{"retention_class":"FINANCIAL_7Y"}`),
		InitialLogic:   json.RawMessage(`{"kind":"MATCH"}`), InitialEffectiveFrom: "2026-09-01",
	}
}

func TestDefinitionValidation(t *testing.T) {
	d := validDef()
	if err := d.Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	if d.Assertions[0] != "COMPLETENESS" {
		t.Errorf("assertions must be normalised, got %v", d.Assertions)
	}

	mutations := map[string]func(*CreateControlDefinitionRequest){
		"bad code":              func(r *CreateControlDefinitionRequest) { r.ControlCode = "AR-1" },
		"no owner":              func(r *CreateControlDefinitionRequest) { r.OwnerRole = "" },
		"no assertion":          func(r *CreateControlDefinitionRequest) { r.Assertions = nil },
		"unknown assertion":     func(r *CreateControlDefinitionRequest) { r.Assertions = []string{"VIBES"} },
		"bad risk tier":         func(r *CreateControlDefinitionRequest) { r.RiskTier = "HIGH" },
		"bad frequency":         func(r *CreateControlDefinitionRequest) { r.Frequency = "WEEKLY" },
		"key without certifier": func(r *CreateControlDefinitionRequest) { r.CertifierRole = "" },
		"non-key close gating":  func(r *CreateControlDefinitionRequest) { r.RiskTier = "STANDARD"; r.CloseGating = true },
		"no source population":  func(r *CreateControlDefinitionRequest) { r.SourceSpec = nil },
		"no target":             func(r *CreateControlDefinitionRequest) { r.TargetSpec = json.RawMessage(`{}`) },
		"no evidence policy":    func(r *CreateControlDefinitionRequest) { r.EvidencePolicy = nil },
		"no logic":              func(r *CreateControlDefinitionRequest) { r.InitialLogic = nil },
		"bad effective date":    func(r *CreateControlDefinitionRequest) { r.InitialEffectiveFrom = "09/01/2026" },
		"invalid json scope":    func(r *CreateControlDefinitionRequest) { r.Scope = json.RawMessage(`{`) },
	}
	for name, mutate := range mutations {
		r := validDef()
		mutate(&r)
		if err := r.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: expected ErrInvalidArgument, got %v", name, err)
		}
	}

	// A preventive control tests an expected condition; it needs no target population.
	p := validDef()
	p.ControlType, p.TargetSpec, p.RiskTier, p.CloseGating = "PREVENTIVE", nil, "STANDARD", false
	if err := p.Validate(); err != nil {
		t.Errorf("preventive control without target_spec should be valid: %v", err)
	}
}

func TestDigestLogic_CanonicalAndSensitive(t *testing.T) {
	a, _ := DigestLogic(json.RawMessage(`{"b":2,"a":{"y":1,"x":[1,2]}}`))
	b, _ := DigestLogic(json.RawMessage(`{ "a": {"x":[1,2], "y":1}, "b": 2 }`))
	c, _ := DigestLogic(json.RawMessage(`{"b":2,"a":{"y":1,"x":[2,1]}}`))
	if a != b {
		t.Errorf("key order/whitespace must not change the digest: %s vs %s", a, b)
	}
	if a == c {
		t.Error("a semantic change must change the digest")
	}
	if !ValidDigest(a) {
		t.Errorf("digest %q is not well-formed", a)
	}
	if _, err := DigestLogic(json.RawMessage(`{`)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("invalid JSON must be ErrInvalidArgument, got %v", err)
	}
}

func TestTolerancePolicyValidation(t *testing.T) {
	ok := CreateTolerancePolicyRequest{LegalEntityID: "e", Metric: "FIN-CTRL-001", AbsoluteTolerance: "0.01",
		Currency: "USD", Rationale: "rounding", EffectiveFrom: "2026-09-01", ApprovedBy: "approver"}
	if err := ok.Validate("maker"); err != nil {
		t.Fatalf("valid tolerance rejected: %v", err)
	}
	cases := map[string]func(*CreateTolerancePolicyRequest){
		"negative absolute":          func(r *CreateTolerancePolicyRequest) { r.AbsoluteTolerance = "-1" },
		"non-numeric":                func(r *CreateTolerancePolicyRequest) { r.AbsoluteTolerance = "abc" },
		"too many fractional digits": func(r *CreateTolerancePolicyRequest) { r.AbsoluteTolerance = "0.0000000000001" },
		"pct without base":           func(r *CreateTolerancePolicyRequest) { r.PercentageTolerance = "0.5" },
		"bad currency":               func(r *CreateTolerancePolicyRequest) { r.Currency = "usd" },
		"no rationale":               func(r *CreateTolerancePolicyRequest) { r.Rationale = "" },
		"bad date basis":             func(r *CreateTolerancePolicyRequest) { r.DateBasis = "LUNAR" },
		"no approver":                func(r *CreateTolerancePolicyRequest) { r.ApprovedBy = "" },
	}
	for name, mutate := range cases {
		r := ok
		mutate(&r)
		if err := r.Validate("maker"); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: expected ErrInvalidArgument, got %v", name, err)
		}
	}
	// Scenario 04 support: the person changing a tolerance cannot approve their own change.
	self := ok
	self.ApprovedBy = "maker"
	if err := self.Validate("maker"); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("self-approved tolerance must be refused, got %v", err)
	}
	// 0.01 must survive exactly — no float artefacts.
	precise := ok
	precise.AbsoluteTolerance = "0.1000000000001"[:14]
	if err := precise.Validate("maker"); err != nil {
		t.Errorf("12-digit precision must be accepted: %v", err)
	}
}

func TestMaterialityValidation(t *testing.T) {
	ok := CreateMaterialityPolicyRequest{LegalEntityID: "e", ReportingBasis: "US_GAAP", AmountThreshold: "1000",
		AggregateThreshold: "5000", Currency: "USD", EffectiveFrom: "2026-09-01", ApprovedBy: "cfo"}
	if err := ok.Validate("maker"); err != nil {
		t.Fatalf("valid materiality rejected: %v", err)
	}
	// §11 aggregation: a looser aggregate than individual threshold is incoherent.
	bad := ok
	bad.AggregateThreshold = "500"
	if err := bad.Validate("maker"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("aggregate < individual must be refused, got %v", err)
	}
	self := ok
	self.ApprovedBy = "maker"
	if err := self.Validate("maker"); !errors.Is(err, ErrSelfApproval) {
		t.Errorf("self-approval must be refused, got %v", err)
	}
}

func TestRunRequestValidation(t *testing.T) {
	ok := CreateRunRequest{ControlDefinitionID: "d", LegalEntityID: "e", TriggerType: "DAILY"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}
	cases := map[string]CreateRunRequest{
		"no definition":             {LegalEntityID: "e", TriggerType: "DAILY"},
		"bad trigger":               {ControlDefinitionID: "d", LegalEntityID: "e", TriggerType: "WHENEVER"},
		"on-demand without reason":  {ControlDefinitionID: "d", LegalEntityID: "e", TriggerType: "ON_DEMAND"},
		"rerun without reason":      {ControlDefinitionID: "d", LegalEntityID: "e", TriggerType: "DAILY", PriorRunID: "p"},
		"period-end without period": {ControlDefinitionID: "d", LegalEntityID: "e", TriggerType: "PERIOD_END"},
	}
	for name, r := range cases {
		if err := r.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: expected ErrInvalidArgument, got %v", name, err)
		}
	}
	good := CreateRunRequest{ControlDefinitionID: "d", LegalEntityID: "e", TriggerType: "ON_DEMAND", Reason: "audit rerun"}
	if err := good.Validate(); err != nil {
		t.Errorf("on-demand with reason should be valid: %v", err)
	}
}
