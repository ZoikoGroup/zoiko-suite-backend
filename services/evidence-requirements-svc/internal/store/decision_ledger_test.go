package store

import (
	"encoding/json"
	"testing"
	"time"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// DL-6: every evaluation outcome is a ledger record whose basis is the
// requirement set it was decided under; artifacts are counted, never copied.
func TestEvaluationRecord_OutcomesAndRequirementSetBasis(t *testing.T) {
	base := domain.EvidenceEvaluation{EvaluationID: "3c1d7e52-0d0b-4c55-9a4e-2b1f8d0e6a11", TenantID: "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222", DomainCode: "FINANCE", ActionType: "PAYMENT_RELEASE",
		UnmetPayload:            json.RawMessage(`[{"evidence_requirement_id":"r-2","evidence_type":"SIGNATURE","reason":"not presented"}]`),
		PresentArtifactsPayload: json.RawMessage(`[{"evidence_type":"DOCUMENT","reference_id":"doc-secret-ref"}]`),
		RequirementIDs:          []string{"r-2", "r-1"}, EvaluatedAt: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC),
		EvaluatedForPrincipalID: "person-1", CorrelationID: "corr-1"}
	for _, o := range []domain.Outcome{domain.OutcomeSatisfied, domain.OutcomeMissing, domain.OutcomeNoRequirementsDefined} {
		e := base
		e.Outcome = o
		rec := evaluationRecord(&e)
		if err := rec.Normalize(); err != nil {
			t.Fatalf("%s: the ledger would refuse it: %v", o, err)
		}
		if rec.Outcome != string(o) || rec.DecisionID != e.EvaluationID || rec.ActionType != "EVIDENCE_PAYMENT_RELEASE" ||
			rec.Domain != "GOVERNANCE_COMPLIANCE" || rec.EvaluationContext[decisionledger.OnBehalfOfKey] != "person-1" {
			t.Fatalf("%s: record %+v", o, rec)
		}
		if rec.EvaluationContext["artifacts_presented"] != 1 {
			t.Fatalf("%s: artifacts must be counted, not copied: %v", o, rec.EvaluationContext)
		}
		raw, _ := json.Marshal(rec)
		if json.Valid(raw) && containsString(string(raw), "doc-secret-ref") {
			t.Fatalf("%s: a presented artifact reference reached the ledger", o)
		}
	}
	a, b := base, base
	b.RequirementIDs = []string{"r-1", "r-2"}
	if evaluationRecord(&a).RuleBasis != evaluationRecord(&b).RuleBasis {
		t.Fatal("the same requirement set in another order is the same version")
	}
	b.RequirementIDs = []string{"r-1", "r-3"}
	if evaluationRecord(&a).RuleBasis == evaluationRecord(&b).RuleBasis {
		t.Fatal("a different requirement set is a different version")
	}
}

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
