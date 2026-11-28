// Package ledger turns an authorization decision into the governance-ledger
// record authorization-svc relays to governance-decision-log-svc (tracker
// "Assignment 25 Nov 2026", step DL-3; Doc 01 s7, Doc 03 s8.3).
//
// Decision D-DL-1 (user, 25 Nov 2026): only DENY and REQUIRE_APPROVAL are
// relayed. Every PERMIT and STEP_UP stays only in access_decision_log, which
// already holds every decision (GET /v1/access-decisions). The record is
// written to decision_log_outbox in the decision's own transaction
// (store.PgStore.SetDecisionLedger) and delivered by
// identity-envelope/decisionledger's relay as svc-authorization.
package ledger

import (
	"strings"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/authorization-svc/internal/domain"
)

// PlatformScope is governance-decision-log-svc's legal_entity_id for a
// decision not scoped to one legal entity (its platformScopeSentinel).
const PlatformScope = "PLATFORM"

// Domain is the decision-log domain every authorization decision is filed
// under: the action it evaluated names finance, payroll or other domains, but
// the verdict is about access.
const Domain = "ORGANIZATION_ACCESS"

// verdict is the canonical decision of d: Decision when the evaluation set
// one, otherwise DENY for a DENIED admin-gate check (checkPermission records
// GRANTED / DENIED only).
func verdict(d domain.AccessDecisionLog) string {
	if d.Decision != "" {
		return d.Decision
	}
	if d.DecisionOutcome == domain.OutcomeDenied {
		return domain.CanonicalDecisionDeny
	}
	return ""
}

// Builder returns the record builder for store.PgStore.SetDecisionLedger.
// It answers nil (nothing to relay) for:
//   - a PERMIT or STEP_UP (D-DL-1);
//   - a decision with no tenant: the ledger is tenant-scoped, and the
//     tenantless decision stays in access_decision_log;
//   - a decision about writerPrincipal itself. That is the ledger asking
//     whether this relay may append; were it denied, relaying the denial
//     would ask the same question again, without end, and could not be
//     recorded anyway.
//
// A tenant-scope check (the admin gates record the tenant id as the scope)
// and a platform-scope check (platformScopeEntityID) are filed under
// PlatformScope: they belong to no legal entity, and the ledger's own
// authorization check could not resolve a tenant id as an entity.
func Builder(writerPrincipal, platformScopeEntityID string) func(domain.AccessDecisionLog) (*decisionledger.Record, error) {
	return func(d domain.AccessDecisionLog) (*decisionledger.Record, error) {
		v := verdict(d)
		if v != domain.CanonicalDecisionDeny && v != domain.CanonicalDecisionRequireApproval {
			return nil, nil
		}
		if d.TenantID == nil || *d.TenantID == "" || d.PrincipalID == writerPrincipal {
			return nil, nil
		}
		rec := Record(d, v)
		if d.LegalEntityID == *d.TenantID || (platformScopeEntityID != "" && d.LegalEntityID == platformScopeEntityID) {
			rec.LegalEntityID = PlatformScope
			rec.EvaluationContext["evaluated_scope"] = d.LegalEntityID
		}
		return rec, nil
	}
}

// Record builds the ledger record of a relayed decision. The attributes the
// evaluation saw are represented by their digest only, never their values.
func Record(d domain.AccessDecisionLog, v string) *decisionledger.Record {
	basis := []string{}
	if d.PolicySetVersion != "" {
		basis = append(basis, d.PolicySetVersion)
	}
	if len(d.ReasonCodes) > 0 {
		basis = append(basis, "reasons:"+strings.Join(d.ReasonCodes, ","))
	}
	if d.DecisionBasis != "" {
		basis = append(basis, "basis:"+d.DecisionBasis)
	}
	if len(basis) == 0 {
		basis = append(basis, "authorization")
	}
	eval := map[string]any{
		decisionledger.OnBehalfOfKey: d.PrincipalID,
		"decision_engine":            "authorization",
		"access_decision_id":         d.AccessDecisionID,
		"evaluated_action":           d.ActionType,
		"decision":                   v,
		"decision_outcome":           d.DecisionOutcome,
		"decision_basis":             d.DecisionBasis,
		"reason_codes":               nonNil(d.ReasonCodes),
		"obligations":                nonNil(d.Obligations),
	}
	for k, val := range map[string]string{
		"policy_set_version": d.PolicySetVersion, "attributes_digest": d.AttributesDigest,
		"session_assurance": d.SessionAssurance, "delegator_principal_id": d.OnBehalfOf,
		"delegation_id": d.DelegationID, "resource_version": d.ResourceVersion,
	} {
		if val != "" {
			eval[k] = val
		}
	}
	correlation := d.CorrelationID
	if correlation == "" || len(correlation) > 64 {
		// The ledger requires one; the decision is its own trace.
		correlation = "access-decision:" + d.AccessDecisionID
	}
	title := "Access denied: " + d.ActionType
	if v == domain.CanonicalDecisionRequireApproval {
		title = "Approval required: " + d.ActionType
	}
	return &decisionledger.Record{
		DecisionID:        d.AccessDecisionID,
		TenantID:          *d.TenantID,
		LegalEntityID:     d.LegalEntityID,
		ActionType:        "ACCESS_" + d.ActionType,
		Outcome:           v,
		RuleBasis:         strings.Join(basis, " "),
		ActionSubjectType: d.ResourceType,
		ActionSubjectID:   d.ResourceID,
		EvaluationContext: eval,
		CorrelationID:     correlation,
		DecidedAt:         d.DecidedAt,
		Title:             title,
		Domain:            Domain,
	}
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
