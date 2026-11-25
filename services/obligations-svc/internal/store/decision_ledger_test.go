package store

import (
	"strings"
	"testing"
	"time"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/obligations-svc/internal/domain"
)

func ledgerClock() *domain.Clock {
	return &domain.Clock{ClockID: "8d1f2c8e-1a3b-4c5d-9e7f-0a1b2c3d4e5f", ObligationID: "ob-1", TenantID: "11111111-1111-1111-1111-111111111111",
		LegalEntityID: "22222222-2222-2222-2222-222222222222", ClockClass: domain.ClockClass("HARD_LEGAL"),
		DueAt: time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC), RowVersion: 3}
}

// DL-5: an obligation raised by evaluate is RAISED with the rule, pack and
// calendar version it was computed under.
func TestEvaluateRecord_RaisedWithItsBasis(t *testing.T) {
	rec := evaluateRecord(ledgerClock(), domain.StartClockParams{
		JurisdictionCode: "GB", RuleObligationCode: "VAT_RETURN", Actor: "person-1", CorrelationID: "corr-1", CausationID: "cause-1",
		Calc: domain.Calculation{DecisionID: "calc-1", ObligationRuleID: "rule-9", RuleVersion: 2, PackRef: "jur.gb.tax", PackVersion: "2026.08.1",
			CalendarCode: "GB-ENG", CalendarVersion: 4},
	})
	if err := rec.Normalize(); err != nil {
		t.Fatalf("the ledger would refuse it: %v", err)
	}
	if rec.ActionType != "OBLIGATION_EVALUATE" || rec.Outcome != "RAISED" || rec.DecisionID != "obl-raise-8d1f2c8e-1a3b-4c5d-9e7f-0a1b2c3d4e5f" ||
		rec.RuleBasis != "jurisdiction:GB rule:rule-9@v2 pack:jur.gb.tax@2026.08.1 calendar:GB-ENG@4" || rec.CausationID != "cause-1" ||
		rec.EvaluationContext[decisionledger.OnBehalfOfKey] != "person-1" || rec.LegalEntityID != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("record %+v", rec)
	}
}

// DL-5: a waiver is WAIVED with its reason and the approval it rests on; each
// waiver of a clock version is its own decision.
func TestWaiveRecord_WaivedWithReasonAndApproval(t *testing.T) {
	code, text, ref, by := "REGULATOR_EXTENSION", "the regulator granted an extension", "6f1e7a3c-0000-4000-8000-000000000001", "approver-2"
	rec := waiveRecord(ledgerClock(), domain.ClockCommandParams{ClockID: "c", Command: domain.ClockCommandWaive, Actor: "person-1",
		CorrelationID: strings.Repeat("x", 70), ReasonCode: &code, ReasonText: &text, ApprovalReference: &ref, ApprovedBy: &by})
	if err := rec.Normalize(); err != nil {
		t.Fatalf("the ledger would refuse it: %v", err)
	}
	if rec.ActionType != "OBLIGATION_CLOCK_WAIVE" || rec.Outcome != "WAIVED" || rec.DecisionID != "obl-waive-8d1f2c8e-1a3b-4c5d-9e7f-0a1b2c3d4e5f-v3" ||
		rec.RuleBasis != "reason:REGULATOR_EXTENSION approval:"+ref || rec.WorkflowInstanceID != ref ||
		rec.EvaluationContext["reason_text"] != text || rec.EvaluationContext["approved_by"] != by {
		t.Fatalf("record %+v", rec)
	}
	if !strings.HasPrefix(rec.CorrelationID, "obligation-clock:") {
		t.Fatalf("an over-long correlation falls back to the clock: %q", rec.CorrelationID)
	}
}
