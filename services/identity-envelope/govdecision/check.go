package govdecision

import (
	"encoding/json"
	"strings"
	"time"
)

// Expectation is what an enforcement point knows about the decision it asked
// for. Check compares the decision with it field by field (Doc 02 §2.3).
type Expectation struct {
	TenantID         string
	LegalEntityID    string
	ActorPrincipalID string // "" skips the actor comparison (a system re-check)
	Action           string
	Resource         Resource
	OwningService    string
	PolicyFamily     string // "" skips the family comparison
	InputFingerprint string // "" skips the fingerprint comparison
	// Now is the enforcement time: a decision evaluated after it (beyond
	// ClockSkew) is future-dated, and one whose valid_until is not after it
	// is expired.
	Now       time.Time
	ClockSkew time.Duration
	// Keys verifies the integrity assertion. Nil skips it, which is right only
	// where the transport already proves the issuer (an mTLS call answered by
	// the owning service itself).
	Keys KeySet
}

// Verdict is what an enforcement point must act on.
type Verdict struct {
	// Outcome is the decision's outcome when every check passed, and ERROR
	// otherwise.
	Outcome string
	// Problems lists each failed check; empty when the decision is trusted.
	Problems []string
	// Decision is the decoded decision, nil when it could not be decoded.
	Decision *Decision
}

// Trusted reports whether every check passed.
func (v Verdict) Trusted() bool { return len(v.Problems) == 0 }

// Check validates a decision's raw JSON against what the caller expected. Any
// problem makes the verdict ERROR, whatever the decision says: an unknown
// outcome, a missing field, a wrong tenant, entity, actor, action, resource
// or owning service, a future-dated or expired decision, a mismatched
// fingerprint or a signature that does not verify (C02-P0-01, -03, -13).
func Check(raw []byte, exp Expectation) Verdict {
	var d Decision
	if err := json.Unmarshal(raw, &d); err != nil {
		return Verdict{Outcome: Error, Problems: []string{"decision is not valid JSON: " + err.Error()}}
	}
	var problems []string
	fail := func(p string) { problems = append(problems, p) }

	if !KnownOutcome(d.Outcome) {
		fail("unknown outcome " + quote(d.Outcome))
	}
	if major(d.ContractVersion) != major(ContractVersion) {
		fail("contract_version " + quote(d.ContractVersion) + " is not " + ContractVersion)
	}
	for name, v := range map[string]string{
		"decision_id": d.DecisionID, "owning_service": d.OwningService, "rule_id": d.RuleID,
		"tenant_id": d.TenantID, "legal_entity_id": d.LegalEntityID, "action": d.Action,
		"input_fingerprint": d.InputFingerprint, "evidence_ref": d.EvidenceRef,
	} {
		if strings.TrimSpace(v) == "" {
			fail(name + " is missing")
		}
	}
	if d.EvaluatedAt.IsZero() {
		fail("evaluated_at is missing")
	}
	if d.ValidUntil.IsZero() {
		fail("valid_until is missing")
	}
	// Every decision says why, including one no rule produced (Doc 02 §2.2:
	// "undefined rule must carry explicit reason").
	if len(d.ReasonCodes) == 0 {
		fail("reason_codes is empty")
	}

	mismatch := func(field, got, want string) {
		if want != "" && !strings.EqualFold(got, want) {
			fail(field + " " + quote(got) + " does not match " + quote(want))
		}
	}
	mismatch("tenant_id", d.TenantID, exp.TenantID)
	mismatch("legal_entity_id", d.LegalEntityID, exp.LegalEntityID)
	mismatch("actor_principal_id", d.ActorPrincipalID, exp.ActorPrincipalID)
	mismatch("action", d.Action, exp.Action)
	mismatch("owning_service", d.OwningService, exp.OwningService)
	mismatch("policy_family", d.PolicyFamily, exp.PolicyFamily)
	mismatch("input_fingerprint", d.InputFingerprint, exp.InputFingerprint)
	if exp.Resource != (Resource{}) && d.Resource != exp.Resource {
		fail("resource does not match the resource asked about (type, id and version must all match)")
	}

	if !exp.Now.IsZero() {
		skew := exp.ClockSkew
		if skew <= 0 {
			skew = 30 * time.Second
		}
		if d.EvaluatedAt.After(exp.Now.Add(skew)) {
			fail("decision is future-dated")
		}
		if !d.ValidUntil.IsZero() && !d.ValidUntil.After(exp.Now) {
			fail(ReasonDecisionExpired)
		}
	}
	if exp.Keys != nil {
		if err := exp.Keys.Verify(raw); err != nil {
			fail(ReasonIntegrityFailed + ": " + err.Error())
		}
	}

	if len(problems) > 0 {
		return Verdict{Outcome: Error, Problems: problems, Decision: &d}
	}
	return Verdict{Outcome: d.Outcome, Decision: &d}
}

func major(v string) string {
	if i := strings.IndexByte(v, '.'); i >= 0 {
		return v[:i]
	}
	return v
}

func quote(s string) string { return "\"" + s + "\"" }
