package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/identity-envelope/decisionledger"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

func ledgerReq() *http.Request {
	r := httptest.NewRequest("POST", "/v1/admin/packs/GB-PAYROLL/versions/3/certify", nil)
	r.Header.Set("X-Tenant-Id", "11111111-1111-1111-1111-111111111111")
	r.Header.Set("X-Correlation-ID", "corr-jur-1")
	r.Header.Set("Idempotency-Key", "key-1")
	return r
}

// DL-4: every governed pack, interpretation, import, calendar and payment-term
// decision maps to a ledger record with its version as rule_basis; any other
// registry event records nothing.
func TestLedgerRecord_GovernedDecisionsOnly(t *testing.T) {
	pack := map[string]any{"pack_ref": "GB-PAYROLL", "version": "3", "certification_id": "cert-1", events.AggregateVersionKey: int64(4)}
	cases := []struct {
		event, action, outcome, basis string
		payload                       map[string]any
	}{
		{events.EventPackCertified, "JURISDICTION_PACK_CERTIFY", "CERTIFIED", "GB-PAYROLL@3", pack},
		{events.EventPackReleased, "JURISDICTION_PACK_RELEASE", "RELEASED", "GB-PAYROLL@3", pack},
		{events.EventPackBlocked, "JURISDICTION_PACK_BLOCK", "BLOCKED", "GB-PAYROLL@3", pack},
		{events.EventPackUnblocked, "JURISDICTION_PACK_UNBLOCK", "UNBLOCKED", "GB-PAYROLL@3", pack},
		{events.EventPackWithdrawn, "JURISDICTION_PACK_WITHDRAW", "WITHDRAWN", "GB-PAYROLL@3", pack},
		{events.EventPackRolledBack, "JURISDICTION_PACK_ROLLBACK", "ROLLED_BACK", "GB-PAYROLL@3", pack},
		{events.EventPackSuperseded, "JURISDICTION_PACK_SUPERSEDE", "SUPERSEDED", "GB-PAYROLL@3", pack},
		{events.EventInterpretationApproved, "JURISDICTION_INTERPRETATION_APPROVE", "APPROVED", "interpretation:agg-1", map[string]any{"jurisdiction_id": "j-1"}},
		{EventImportApproved, "JURISDICTION_IMPORT_APPROVE", "APPROVED", "import:agg-1", map[string]any{"added": 2}},
		{events.EventBusinessCalendarUpdated, "JURISDICTION_CALENDAR_PUBLISH", "PUBLISHED", "calendar:GB-ENG@2", map[string]any{"calendar_code": "GB-ENG", "calendar_version": 2}},
		{events.EventPaymentTermActivated, "JURISDICTION_PAYMENT_TERM_APPROVE", "ACTIVATED", "payment_term:NET30@1", map[string]any{"term_code": "NET30", "term_version": 1}},
		{events.EventPaymentTermActivated, "JURISDICTION_PAYMENT_TERM_SUPERSEDE", "ACTIVATED", "payment_term:NET30@2", map[string]any{"term_code": "NET30", "term_version": 2, "supersedes_version": 1}},
	}
	for _, c := range cases {
		rec := ledgerRecord(ledgerReq(), c.event, "agg-1", "person-7", c.payload)
		if rec == nil {
			t.Fatalf("%s: no record", c.event)
		}
		if rec.ActionType != c.action || rec.Outcome != c.outcome || rec.RuleBasis != c.basis {
			t.Fatalf("%s: %s %s %s, want %s %s %s", c.event, rec.ActionType, rec.Outcome, rec.RuleBasis, c.action, c.outcome, c.basis)
		}
		if rec.LegalEntityID != LedgerPlatformScope || rec.TenantID != "11111111-1111-1111-1111-111111111111" ||
			rec.CorrelationID != "corr-jur-1" || rec.EvaluationContext[decisionledger.OnBehalfOfKey] != "person-7" {
			t.Fatalf("%s: context %+v", c.event, rec)
		}
		if err := rec.Normalize(); err != nil {
			t.Fatalf("%s: the ledger would refuse it: %v", c.event, err)
		}
	}
	for _, other := range []string{events.EventPackCreated, events.EventSourceCaptured, events.EventPackVersionDrafted, events.EventRuleVersioned} {
		if rec := ledgerRecord(ledgerReq(), other, "agg-1", "person-7", pack); rec != nil {
			t.Fatalf("%s is not a governed decision: %+v", other, rec)
		}
	}
}

func TestLedgerRecord_DecisionIDIsStablePerChange(t *testing.T) {
	p := map[string]any{"pack_ref": "GB-PAYROLL", "version": "3", events.AggregateVersionKey: int64(4)}
	a := ledgerRecord(ledgerReq(), events.EventPackBlocked, "GB-PAYROLL@3", "u", p)
	b := ledgerRecord(ledgerReq(), events.EventPackBlocked, "GB-PAYROLL@3", "u", p)
	if a.DecisionID != b.DecisionID || len(a.DecisionID) > 64 {
		t.Fatalf("same change, ids %q %q", a.DecisionID, b.DecisionID)
	}
	p2 := map[string]any{"pack_ref": "GB-PAYROLL", "version": "3", events.AggregateVersionKey: int64(6)}
	if c := ledgerRecord(ledgerReq(), events.EventPackBlocked, "GB-PAYROLL@3", "u", p2); c.DecisionID == a.DecisionID {
		t.Fatal("a later block of the same version is a new decision")
	}
	if u := ledgerRecord(ledgerReq(), events.EventPackUnblocked, "GB-PAYROLL@3", "u", p); u.DecisionID == a.DecisionID {
		t.Fatal("block and unblock are two decisions")
	}
}
