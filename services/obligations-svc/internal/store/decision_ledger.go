package store

// Decision capture (tracker "Assignment 25 Nov 2026", step DL-5; Doc 01 s7).
//
// Two decisions of the Obligations engine reach governance-decision-log-svc as
// svc-obligations: an obligation raised by POST /v1/obligations/evaluate (its
// clock started from a jurisdiction calculation: RAISED) and a clock waiver
// (WAIVED, with its reason and the approval it rests on). Each record goes into
// decision_log_outbox (000017) in the transaction of the clock change, and
// identity-envelope/decisionledger's relay delivers it.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/obligations-svc/internal/domain"
)

// EvaluateClockKeyPrefix marks a clock started by POST /v1/obligations/evaluate
// (handler: "evaluate:" + Idempotency-Key).
const EvaluateClockKeyPrefix = "evaluate:"

// SetDecisionLedger turns decision capture on or off (DL-5).
func (s *PgStore) SetDecisionLedger(on bool) { s.ledgerOn = on }

func correlationOr(corr, fallback string) string {
	if corr == "" || len(corr) > 64 {
		if len(fallback) > 64 {
			return fallback[:64]
		}
		return fallback
	}
	return corr
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// evaluateRecord is the ledger record of an obligation raised by evaluate: the
// rule version, pack and calendar version the due date was computed under are
// its basis.
func evaluateRecord(c *domain.Clock, p domain.StartClockParams) decisionledger.Record {
	basis := []string{}
	if p.JurisdictionCode != "" {
		basis = append(basis, "jurisdiction:"+p.JurisdictionCode)
	}
	if p.Calc.ObligationRuleID != "" {
		basis = append(basis, fmt.Sprintf("rule:%s@v%d", p.Calc.ObligationRuleID, p.Calc.RuleVersion))
	} else if p.RuleObligationCode != "" {
		basis = append(basis, "rule:"+p.RuleObligationCode)
	}
	if p.Calc.PackRef != "" {
		basis = append(basis, "pack:"+p.Calc.PackRef+"@"+p.Calc.PackVersion)
	}
	if p.Calc.CalendarCode != "" {
		basis = append(basis, fmt.Sprintf("calendar:%s@%d", p.Calc.CalendarCode, p.Calc.CalendarVersion))
	}
	if len(basis) == 0 {
		basis = append(basis, "obligation-calculation:"+p.Calc.DecisionID)
	}
	eval := map[string]any{
		decisionledger.OnBehalfOfKey: p.Actor,
		"decision_engine":            "obligations",
		"obligation_id":              c.ObligationID,
		"clock_id":                   c.ClockID,
		"clock_class":                string(c.ClockClass),
		"due_at":                     c.DueAt.UTC().Format(time.RFC3339),
		"calculation_decision_id":    p.Calc.DecisionID,
	}
	return decisionledger.Record{
		DecisionID:          "obl-raise-" + c.ClockID,
		TenantID:            c.TenantID,
		LegalEntityID:       c.LegalEntityID,
		ActionType:          "OBLIGATION_EVALUATE",
		Outcome:             "RAISED",
		RuleBasis:           strings.Join(basis, " "),
		JurisdictionContext: p.JurisdictionCode,
		ActionSubjectType:   "obligation",
		ActionSubjectID:     c.ObligationID,
		EvaluationContext:   eval,
		CorrelationID:       correlationOr(p.CorrelationID, "obligation-clock:"+c.ClockID),
		CausationID:         p.CausationID,
		DecidedAt:           time.Now().UTC(),
		Title:               "Obligation raised: " + p.RuleObligationCode,
		Domain:              "GOVERNANCE_COMPLIANCE",
	}
}

// waiveRecord is the ledger record of a clock waiver: its reason and the
// approval it rests on are its basis (a waiver needs an approved workflow,
// WFC-05).
func waiveRecord(c *domain.Clock, p domain.ClockCommandParams) decisionledger.Record {
	basis := []string{}
	if code := strOr(p.ReasonCode); code != "" {
		basis = append(basis, "reason:"+code)
	}
	if ref := strOr(p.ApprovalReference); ref != "" {
		basis = append(basis, "approval:"+ref)
	}
	if len(basis) == 0 {
		basis = append(basis, "waiver")
	}
	eval := map[string]any{
		decisionledger.OnBehalfOfKey: p.Actor,
		"decision_engine":            "obligations",
		"obligation_id":              c.ObligationID,
		"clock_id":                   c.ClockID,
		"clock_class":                string(c.ClockClass),
		"due_at":                     c.DueAt.UTC().Format(time.RFC3339),
	}
	if p.ReasonText != nil {
		eval["reason_text"] = *p.ReasonText
	}
	if p.ApprovedBy != nil {
		eval["approved_by"] = *p.ApprovedBy
	}
	if len(p.EvidenceRefs) > 0 {
		eval["evidence_refs"] = p.EvidenceRefs
	}
	return decisionledger.Record{
		DecisionID:         fmt.Sprintf("obl-waive-%s-v%d", c.ClockID, c.RowVersion),
		TenantID:           c.TenantID,
		LegalEntityID:      c.LegalEntityID,
		ActionType:         "OBLIGATION_CLOCK_WAIVE",
		Outcome:            "WAIVED",
		RuleBasis:          strings.Join(basis, " "),
		ActionSubjectType:  "obligation_clock",
		ActionSubjectID:    c.ClockID,
		EvaluationContext:  eval,
		CorrelationID:      correlationOr(p.CorrelationID, "obligation-clock:"+c.ClockID),
		WorkflowInstanceID: strOr(p.ApprovalReference),
		DecidedAt:          time.Now().UTC(),
		Title:              "Obligation clock waived",
		Domain:             "GOVERNANCE_COMPLIANCE",
	}
}

// recordDecisionTx queues rec in tx when decision capture is on. A record that
// cannot be queued fails the transaction: the change and its evidence commit
// together or not at all.
func (s *PgStore) recordDecisionTx(ctx context.Context, tx pgx.Tx, rec decisionledger.Record) error {
	if !s.ledgerOn {
		return nil
	}
	if err := decisionledger.Enqueue(ctx, tx, rec); err != nil {
		return storeErr(err)
	}
	return nil
}
