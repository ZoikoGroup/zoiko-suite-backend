package store

// Decision capture (tracker "Assignment 25 Nov 2026", step DL-6; Doc 01 s7).
//
// Every evidence evaluation (POST /v1/evidence/evaluate) is a governance
// decision: SATISFIED and NO_REQUIREMENTS_DEFINED let the action proceed,
// MISSING blocks it. Its ledger record goes into decision_log_outbox (000013)
// in the transaction that records the evaluation, and
// identity-envelope/decisionledger's relay delivers it to
// governance-decision-log-svc as svc-evidence-requirements.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// SetDecisionLedger turns decision capture on or off (DL-6).
func (s *PgStore) SetDecisionLedger(on bool) { s.ledgerOn = on }

// requirementSetVersion names the requirement set an evaluation was decided
// under: a digest of its requirement ids in order, so two evaluations under the
// same set share it and a changed catalogue reads as a new version.
func requirementSetVersion(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, ",")))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// evaluationRecord is the ledger record of one evidence evaluation. Only the
// unmet requirements (ids, types, reasons) and the number of artifacts
// presented travel; the artifacts themselves stay here.
func evaluationRecord(e *domain.EvidenceEvaluation) decisionledger.Record {
	var presented []json.RawMessage
	_ = json.Unmarshal(e.PresentArtifactsPayload, &presented)
	var unmet any
	if len(e.UnmetPayload) > 0 {
		_ = json.Unmarshal(e.UnmetPayload, &unmet)
	}
	ids := e.RequirementIDs
	if ids == nil {
		ids = []string{}
	}
	corr := e.CorrelationID
	if corr == "" || len(corr) > 64 {
		corr = "evidence-evaluation:" + e.EvaluationID
	}
	title := "Evidence satisfied: " + e.ActionType
	switch e.Outcome {
	case domain.OutcomeMissing:
		title = "Evidence missing: " + e.ActionType
	case domain.OutcomeNoRequirementsDefined:
		title = "No evidence required: " + e.ActionType
	}
	return decisionledger.Record{
		DecisionID:        e.EvaluationID,
		TenantID:          e.TenantID,
		LegalEntityID:     e.LegalEntityID,
		ActionType:        "EVIDENCE_" + e.ActionType,
		Outcome:           string(e.Outcome),
		RuleBasis:         "requirements:" + e.DomainCode + "/" + e.ActionType + "@" + requirementSetVersion(ids),
		ActionSubjectType: "evidence_evaluation",
		ActionSubjectID:   e.EvaluationID,
		EvaluationContext: map[string]any{
			decisionledger.OnBehalfOfKey: e.EvaluatedForPrincipalID,
			"decision_engine":            "evidence_requirements",
			"domain_code":                e.DomainCode,
			"evaluated_action":           e.ActionType,
			"requirement_ids":            ids,
			"unmet":                      unmet,
			"artifacts_presented":        len(presented),
		},
		CorrelationID: corr,
		DecidedAt:     e.EvaluatedAt,
		Title:         title,
		Domain:        "GOVERNANCE_COMPLIANCE",
	}
}
