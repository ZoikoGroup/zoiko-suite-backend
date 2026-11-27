package ledger

import (
	"strings"
	"testing"
	"time"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/authorization-svc/internal/domain"
)

const platformEntity = "00000000-0000-0000-0000-00000000f001"

func decision(v, outcome string) domain.AccessDecisionLog {
	tenant := "11111111-1111-1111-1111-111111111111"
	return domain.AccessDecisionLog{
		AccessDecisionID: "5f0c7a52-0d0b-4c55-9a4e-2b1f8d0e6a11", // gitleaks:allow -- test fixture UUID, not a secret
		PrincipalID: "a1000000-0000-4000-8000-000000000002",
		LegalEntityID: "22222222-2222-2222-2222-222222222222", ActionType: "zt.payment.release",
		DecisionOutcome: outcome, DecisionBasis: "no_grant", TenantID: &tenant, CorrelationID: "corr-1",
		DecidedAt: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC), Decision: v, PolicySetVersion: "cfg.42",
		ReasonCodes: []string{"NO_GRANT"}, ResourceType: "payment", ResourceID: "PAY-1",
		AttributesDigest: "sha256:abc",
	}
}

// D-DL-1: DENY and REQUIRE_APPROVAL are relayed; PERMIT and STEP_UP are not.
func TestBuilder_RelaysOnlyDenyAndRequireApproval(t *testing.T) {
	b := Builder("svc-authorization", platformEntity)
	cases := []struct {
		name, decision, outcome string
		relayed                 bool
		want                    string
	}{
		{"deny", domain.CanonicalDecisionDeny, domain.OutcomeDenied, true, "DENY"},
		{"require approval", domain.CanonicalDecisionRequireApproval, domain.OutcomeDenied, true, "REQUIRE_APPROVAL"},
		{"admin gate denied (no canonical decision)", "", domain.OutcomeDenied, true, "DENY"},
		{"permit", domain.CanonicalDecisionPermit, domain.OutcomeGranted, false, ""},
		{"step up", domain.CanonicalDecisionStepUp, domain.OutcomeDenied, false, ""},
		{"admin gate granted", "", domain.OutcomeGranted, false, ""},
	}
	for _, c := range cases {
		rec, err := b(decision(c.decision, c.outcome))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (rec != nil) != c.relayed {
			t.Fatalf("%s: relayed=%v, want %v", c.name, rec != nil, c.relayed)
		}
		if rec != nil && rec.Outcome != c.want {
			t.Fatalf("%s: outcome %q, want %q", c.name, rec.Outcome, c.want)
		}
	}
}

func TestBuilder_SkipsTenantlessAndTheRelaysOwnCheck(t *testing.T) {
	b := Builder("svc-authorization", platformEntity)
	d := decision(domain.CanonicalDecisionDeny, domain.OutcomeDenied)
	d.TenantID = nil
	if rec, _ := b(d); rec != nil {
		t.Fatal("a tenantless decision cannot be filed in a tenant ledger")
	}
	d = decision(domain.CanonicalDecisionDeny, domain.OutcomeDenied)
	d.PrincipalID = "svc-authorization"
	if rec, _ := b(d); rec != nil {
		t.Fatal("the relay's own GOVERNANCE_DECISION_RECORD denial must not be relayed (it would loop)")
	}
}

func TestRecord_CarriesBasisActorAndNoAttributeValues(t *testing.T) {
	rec, _ := Builder("svc-authorization", platformEntity)(decision(domain.CanonicalDecisionDeny, domain.OutcomeDenied))
	if err := rec.Normalize(); err != nil {
		t.Fatalf("the ledger would refuse the record: %v", err)
	}
	if rec.DecisionID != "5f0c7a52-0d0b-4c55-9a4e-2b1f8d0e6a11" || rec.ActionType != "ACCESS_zt.payment.release" ||
		rec.RuleBasis != "cfg.42 reasons:NO_GRANT basis:no_grant" || rec.Domain != Domain ||
		rec.ActionSubjectType != "payment" || rec.ActionSubjectID != "PAY-1" || rec.CorrelationID != "corr-1" ||
		!strings.HasPrefix(rec.Title, "Access denied") {
		t.Fatalf("record %+v", rec)
	}
	ec := rec.EvaluationContext
	if ec[decisionledger.OnBehalfOfKey] != "a1000000-0000-4000-8000-000000000002" || ec["attributes_digest"] != "sha256:abc" {
		t.Fatalf("evaluation context %v", ec)
	}
	for k := range ec {
		if k == "attributes" {
			t.Fatal("attribute values must never reach the ledger")
		}
	}
	d := decision(domain.CanonicalDecisionRequireApproval, domain.OutcomeDenied)
	d.CorrelationID = ""
	rec, _ = Builder("svc-authorization", platformEntity)(d)
	if rec.CorrelationID != "access-decision:"+d.AccessDecisionID || !strings.HasPrefix(rec.Title, "Approval required") {
		t.Fatalf("fallback correlation / title: %+v", rec)
	}
}

// A tenant-scope admin gate (scope = the tenant id) and a platform-scope check
// are filed under the ledger's PLATFORM scope; an entity decision keeps its entity.
func TestBuilder_TenantAndPlatformScopeAreFiledUnderPlatform(t *testing.T) {
	b := Builder("svc-authorization", platformEntity)
	d := decision("", domain.OutcomeDenied)
	d.LegalEntityID = *d.TenantID
	rec, _ := b(d)
	if rec.LegalEntityID != PlatformScope || rec.EvaluationContext["evaluated_scope"] != *d.TenantID {
		t.Fatalf("tenant scope: %q %v", rec.LegalEntityID, rec.EvaluationContext["evaluated_scope"])
	}
	d.LegalEntityID = platformEntity
	if rec, _ = b(d); rec.LegalEntityID != PlatformScope {
		t.Fatalf("platform scope: %q", rec.LegalEntityID)
	}
	if rec, _ = b(decision(domain.CanonicalDecisionDeny, domain.OutcomeDenied)); rec.LegalEntityID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("entity scope must be kept: %q", rec.LegalEntityID)
	}
	if err := rec.Normalize(); err != nil {
		t.Fatal(err)
	}
}
