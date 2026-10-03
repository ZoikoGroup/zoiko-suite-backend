package domain

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lifecycleJSON = `{"initial":"SUBMITTED","terminal":["ACCEPTED","REJECTED"],
	"transitions":[["SUBMITTED","PROCESSING"],["SUBMITTED","ACCEPTED"],["SUBMITTED","REJECTED"],["PROCESSING","PROCESSING"],["PROCESSING","ACCEPTED"],["PROCESSING","REJECTED"]],
	"receipt_required":["ACCEPTED"]}`

const einvoiceJSON = `{"family":"EINVOICE_PROFILE","profile":{"channel":"PEPPOL","document_type":"INVOICE","profile_version":"1.0","syntax":"UBL_XML",
	"transmission_mode":"EXCHANGE","timing":{"retry_window_hours":72,"correction_window_days":30},"corrections":["CREDIT_NOTE","CANCEL"],
	"dependencies":[{"kind":"SCHEMA","ref":"peppol.pint.ubl","version":"2026.1","valid_from":"2026-01-01","valid_to":"2026-12-31"},
	                {"kind":"CODE_LIST","ref":"iso.4217","version":"2026.06","valid_from":"2026-06-01","valid_to":null}],
	"semantic_mapping":[{"from":"invoice.number","to":"cbc:ID"},{"from":"invoice.issue_date","to":"cbc:IssueDate"}],
	"lifecycle":` + lifecycleJSON + `}}`

const filingJSON = `{"family":"FILING_PROFILE","profile":{"channel":"HMRC","document_type":"VAT_RETURN","profile_version":"2","transmission_mode":"FILING_VIA_PROVIDER",
	"timing":{"retry_window_hours":24,"correction_window_days":365},"corrections":["AMENDED_FILING"],
	"dependencies":[{"kind":"SCHEMA","ref":"hmrc.vat.mtd","version":"1.0","valid_from":"2026-01-01","valid_to":null}],
	"obligation_code":"VAT_RETURN","approval_roles":["FINANCE_MANAGER","TAX_OFFICER"],"retention_class":"TAX_FILINGS","lifecycle":` + lifecycleJSON + `}}`

func profileOf(t *testing.T, raw string) *SubmissionProfile {
	t.Helper()
	p, err := ParseRuleParameters([]byte(raw))
	require.NoError(t, err)
	require.NotNil(t, p.Profile)
	return p.Profile
}

func TestSubmissionProfiles_ValidExamples(t *testing.T) {
	profileOf(t, einvoiceJSON)
	profileOf(t, filingJSON)
	p, _ := ParseRuleParameters([]byte(einvoiceJSON))
	assert.Equal(t, "1.0", PinnedParameterValue(p))
	assert.True(t, ParameterOnlyFamily(p.Family))
	assert.Equal(t, EInvoiceDomain, FamilyDomain(p.Family))
}

func TestSubmissionProfiles_RejectUnsafeDefinitions(t *testing.T) {
	bad := map[string]string{
		"no dependencies":      strings.Replace(einvoiceJSON, `"dependencies":[`, `"dependencies":[],"x":[`, 1),
		"floaty version":       strings.Replace(einvoiceJSON, `"profile_version":"1.0"`, `"profile_version":"v1"`, 1),
		"unknown mode":         strings.Replace(einvoiceJSON, `"EXCHANGE"`, `"TELEPATHY"`, 1),
		"accepted no receipt":  strings.Replace(einvoiceJSON, `"receipt_required":["ACCEPTED"]`, `"receipt_required":[]`, 1),
		"leaves terminal":      strings.Replace(einvoiceJSON, `["SUBMITTED","PROCESSING"]`, `["ACCEPTED","PROCESSING"]`, 1),
		"back to initial":      strings.Replace(einvoiceJSON, `["PROCESSING","ACCEPTED"]`, `["PROCESSING","SUBMITTED"]`, 1),
		"unreachable state":    strings.Replace(einvoiceJSON, `"terminal":["ACCEPTED","REJECTED"]`, `"terminal":["ACCEPTED","REJECTED","LOST"]`, 1),
		"expiry before start":  strings.Replace(einvoiceJSON, `"valid_to":"2026-12-31"`, `"valid_to":"2025-12-31"`, 1),
		"filing without class": strings.Replace(filingJSON, `"retention_class":"TAX_FILINGS",`, ``, 1),
		"filing with mapping":  strings.Replace(filingJSON, `"retention_class"`, `"semantic_mapping":[{"from":"a","to":"b"}],"retention_class"`, 1),
		"invoice with roles":   strings.Replace(einvoiceJSON, `"timing"`, `"approval_roles":["X_Y"],"timing"`, 1),
		"credentials smuggled": strings.Replace(einvoiceJSON, `"channel":"PEPPOL"`, `"channel":"PEPPOL","api_key":"secret"`, 1),
	}
	for name, raw := range bad {
		_, err := ParseRuleParameters([]byte(raw))
		assert.Error(t, err, name)
	}
}

func TestCheckDependencies_NamesTheExactVersion(t *testing.T) {
	sp := profileOf(t, einvoiceJSON)
	day := func(s string) time.Time { v, _ := parseDay(s); return v }
	assert.Equal(t, OutcomeProfileResolved, sp.CheckDependencies([]DependencyUse{{"peppol.pint.ubl", "2026.1"}}, day("2026-12-31")).Outcome, "valid_to is inclusive")
	exp := sp.CheckDependencies([]DependencyUse{{"peppol.pint.ubl", "2026.1"}}, day("2027-01-01"))
	assert.Equal(t, DependencyExpired, exp.Outcome)
	assert.Contains(t, exp.Message, "peppol.pint.ubl version 2026.1 expired on 2026-12-31")
	early := sp.CheckDependencies([]DependencyUse{{"iso.4217", "2026.06"}}, day("2026-05-31"))
	assert.Equal(t, DependencyExpired, early.Outcome)
	unk := sp.CheckDependencies([]DependencyUse{{"peppol.pint.ubl", "2025.9"}}, day("2026-06-01"))
	assert.Equal(t, DependencyUnknown, unk.Outcome)
	assert.Contains(t, unk.Message, "2026.1")
}

func TestCheckApprovals_SegregationOfDuties(t *testing.T) {
	sp := profileOf(t, filingJSON)
	ok := []Approval{{"FINANCE_MANAGER", "u2"}, {"TAX_OFFICER", "u3"}}
	assert.Equal(t, OutcomeProfileResolved, sp.CheckApprovals("u1", ok).Outcome)
	assert.Equal(t, ApprovalMissing, sp.CheckApprovals("u1", ok[:1]).Outcome)
	assert.Equal(t, ApprovalMissing, sp.CheckApprovals("u2", ok).Outcome, "the preparer cannot approve")
	assert.Equal(t, ApprovalMissing, sp.CheckApprovals("u1", []Approval{{"FINANCE_MANAGER", "u2"}, {"TAX_OFFICER", "u2"}}).Outcome, "one person cannot cover two roles")
}

func TestEvaluateEvent_NoFalseAcceptedNoRegression(t *testing.T) {
	sp := profileOf(t, einvoiceJSON)
	t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	ev := func(status, receipt string, at time.Time) SubmissionEvent {
		return SubmissionEvent{ProviderEventID: "e", Status: status, ReceiptID: receipt, OccurredAt: at}
	}
	assert.Equal(t, DispApplied, sp.EvaluateEvent("SUBMITTED", nil, ev("PROCESSING", "", t0)))
	assert.Equal(t, DispMissingReceipt, sp.EvaluateEvent("PROCESSING", &t0, ev("ACCEPTED", "", t0.Add(time.Hour))), "JUR-NEG-09: no accepted without a receipt")
	assert.Equal(t, DispApplied, sp.EvaluateEvent("PROCESSING", &t0, ev("ACCEPTED", "R-1", t0.Add(time.Hour))))
	assert.Equal(t, DispAfterTerminal, sp.EvaluateEvent("ACCEPTED", &t0, ev("PROCESSING", "", t0.Add(2*time.Hour))), "JUR-NEG-11: no regression from a final state")
	assert.Equal(t, DispAfterTerminal, sp.EvaluateEvent("REJECTED", &t0, ev("ACCEPTED", "R-2", t0.Add(2*time.Hour))))
	assert.Equal(t, DispStale, sp.EvaluateEvent("PROCESSING", &t0, ev("PROCESSING", "", t0.Add(-time.Hour))))
	assert.Equal(t, DispUnknownStatus, sp.EvaluateEvent("SUBMITTED", nil, ev("TELEPORTED", "", t0)))
	assert.Equal(t, DispUnknownStatus, sp.EvaluateEvent("PROCESSING", &t0, ev("SUBMITTED", "", t0.Add(time.Hour))), "cannot go back to the initial state")
	assert.Equal(t, DispApplied, sp.EvaluateEvent("SUBMITTED", nil, ev("ACCEPTED", "R-3", t0)), "a declared shortcut is allowed")
}
