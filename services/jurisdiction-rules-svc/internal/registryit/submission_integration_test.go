//go:build integration

// ZS-JUR-001 Wave 5 (e-invoice s13, filing s14) end to end: profiles are
// authored as typed-parameter rules, packaged, certified and released; a
// submission is registered under the profile in force and authority status
// reports advance it through the profile's lifecycle.
package registryit_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const subLifecycle = `{"initial":"SUBMITTED","terminal":["ACCEPTED","REJECTED"],
	"transitions":[["SUBMITTED","PROCESSING"],["SUBMITTED","ACCEPTED"],["SUBMITTED","REJECTED"],["PROCESSING","PROCESSING"],["PROCESSING","ACCEPTED"],["PROCESSING","REJECTED"]],
	"receipt_required":["ACCEPTED"]}`

const (
	einvoiceProfile = `{"family":"EINVOICE_PROFILE","profile":{"channel":"PEPPOL","document_type":"INVOICE","profile_version":"1.0","syntax":"UBL_XML",
		"transmission_mode":"EXCHANGE","timing":{"retry_window_hours":72,"correction_window_days":30},"corrections":["CREDIT_NOTE","CANCEL"],
		"dependencies":[{"kind":"SCHEMA","ref":"peppol.pint.ubl","version":"2026.1","valid_from":"2026-01-01","valid_to":"2026-12-31"},
		                {"kind":"CODE_LIST","ref":"iso.4217","version":"2026.06","valid_from":"2026-06-01","valid_to":null}],
		"semantic_mapping":[{"from":"invoice.number","to":"cbc:ID"},{"from":"invoice.issue_date","to":"cbc:IssueDate"}],"lifecycle":` + subLifecycle + `}}`
	filingProfile = `{"family":"FILING_PROFILE","profile":{"channel":"HMRC","document_type":"VAT_RETURN","profile_version":"2","transmission_mode":"FILING_VIA_PROVIDER",
		"timing":{"retry_window_hours":24,"correction_window_days":365},"corrections":["AMENDED_FILING"],
		"dependencies":[{"kind":"SCHEMA","ref":"hmrc.vat.mtd","version":"1.0","valid_from":"2026-01-01","valid_to":null}],
		"obligation_code":"VAT_RETURN","approval_roles":["FINANCE_MANAGER","TAX_OFFICER"],"retention_class":"TAX_FILINGS","lifecycle":` + subLifecycle + `}}`
)

func newSubmissionScenario(t *testing.T) *payrollScenario {
	t.Helper()
	return newRulesScenario(t, []payrollSpec{
		{"PEPPOL_INVOICE", pyStart2025, nil, einvoiceProfile, "EINVOICE_PROFILE"},
		{"HMRC_VAT_RETURN", pyStart2025, nil, filingProfile, "STATUTORY_FILING"},
	})
}

const hashA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func (s *payrollScenario) register(t *testing.T, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "kind": "EINVOICE", "profile_code": "PEPPOL_INVOICE", "effective_at": "2026-08-15T00:00:00Z",
		"subject_ref": "INV-1001", "payload_hash": hashA, "dependencies_used": []map[string]string{{"ref": "peppol.pint.ubl", "version": "2026.1"}}}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", "/v1/regulatory-submissions", "billing", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

func (s *payrollScenario) report(t *testing.T, id string, ev map[string]any) map[string]any {
	t.Helper()
	code, r := s.e.do("POST", "/v1/regulatory-submissions/"+id+"/events", "provider-webhook", ev)
	require.Equal(t, 200, code, "%v", r)
	return r
}

func subID(r map[string]any) string {
	return r["submission"].(map[string]any)["submission_id"].(string)
}

func TestSubmissionProfiles_AreResolvedFromTheVerifiedPackWithDependencyChecks(t *testing.T) {
	s := newSubmissionScenario(t)
	s.release(t)
	resolve := func(body map[string]any, want int) map[string]any {
		base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "kind": "EINVOICE", "profile_code": "PEPPOL_INVOICE", "effective_at": "2026-08-15T00:00:00Z"}
		for k, v := range body {
			base[k] = v
		}
		code, r := s.e.do("POST", "/v1/submission-profiles:resolve", "billing", base)
		require.Equal(t, want, code, "%v", r)
		return r
	}
	r := resolve(map[string]any{"dependencies_used": []map[string]string{{"ref": "peppol.pint.ubl", "version": "2026.1"}, {"ref": "iso.4217", "version": "2026.06"}}}, 200)
	p := r["profile"].(map[string]any)
	assert.Equal(t, "PROFILE_RESOLVED", p["outcome"])
	assert.Equal(t, "PEPPOL", p["profile"].(map[string]any)["channel"])
	var kind string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind FROM rule_decision_evidence WHERE decision_id=$1`, r["decision_id"]).Scan(&kind))
	assert.Equal(t, "PROFILE", kind)

	// JUR-NEG-08: the schema is past its validity, and the answer names it exactly.
	r = resolve(map[string]any{"effective_at": "2027-01-05T00:00:00Z", "dependencies_used": []map[string]string{{"ref": "peppol.pint.ubl", "version": "2026.1"}}}, 422)
	chk := r["profile"].(map[string]any)["dependency_check"].(map[string]any)
	assert.Equal(t, "DEPENDENCY_EXPIRED", chk["outcome"])
	assert.Contains(t, chk["message"], "peppol.pint.ubl version 2026.1 expired on 2026-12-31")
	// JUR-NEG-27: a version the profile does not declare.
	r = resolve(map[string]any{"dependencies_used": []map[string]string{{"ref": "peppol.pint.ubl", "version": "2027.1"}}}, 422)
	assert.Equal(t, "DEPENDENCY_UNKNOWN", r["profile"].(map[string]any)["dependency_check"].(map[string]any)["outcome"])
	// Without stated dependencies the profile is returned for the caller to build against.
	resolve(nil, 200)
	// Explicit non-answers.
	r = resolve(map[string]any{"profile_code": "NOPE"}, 200)
	assert.Equal(t, "NO_RULE", r["profile"].(map[string]any)["outcome"])
	r = resolve(map[string]any{"jurisdiction": "ATLANTIS"}, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", r["profile"].(map[string]any)["outcome"])
	resolve(map[string]any{"kind": "TELEGRAM"}, 400)
	// A filing profile is not an e-invoice profile.
	r = resolve(map[string]any{"profile_code": "HMRC_VAT_RETURN"}, 200)
	assert.Equal(t, "NO_RULE", r["profile"].(map[string]any)["outcome"], "an e-invoice lookup does not find a filing rule")
	r = resolve(map[string]any{"kind": "FILING", "profile_code": "HMRC_VAT_RETURN"}, 200)
	assert.Equal(t, "VAT_RETURN", r["profile"].(map[string]any)["profile"].(map[string]any)["obligation_code"])
}

func TestSubmissions_RegisterRefuseAndLifecycle(t *testing.T) {
	s := newSubmissionScenario(t)
	s.release(t)

	// Refused submissions are not registered (JUR-NEG-08), but are recorded as evidence.
	r := s.register(t, map[string]any{"subject_ref": "INV-EXPIRED", "effective_at": "2027-02-01T00:00:00Z"}, 422)
	assert.Equal(t, "DEPENDENCY_EXPIRED", r["profile"].(map[string]any)["outcome"])
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_submissions WHERE subject_ref='INV-EXPIRED'`).Scan(&n))
	assert.Equal(t, 0, n)
	s.register(t, map[string]any{"payload_hash": "abc"}, 400)

	// Registered under the profile in force, with the pack release and a snapshot of the lifecycle.
	r = s.register(t, nil, 201)
	id := subID(r)
	sub := r["submission"].(map[string]any)
	assert.Equal(t, "SUBMITTED", sub["status"])
	assert.NotEmpty(t, sub["artifact_digest"])
	assert.Equal(t, "ACCEPTED", sub["profile_snapshot"].(map[string]any)["lifecycle"].(map[string]any)["terminal"].([]any)[0])

	// Idempotency.
	code, a := s.e.doWithHeaders("POST", "/v1/regulatory-submissions", "billing", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "kind": "EINVOICE",
		"profile_code": "PEPPOL_INVOICE", "effective_at": "2026-08-15T00:00:00Z", "subject_ref": "INV-2002", "payload_hash": hashA}, map[string]string{"Idempotency-Key": "sub-1"})
	mustStatus(t, 201, code, a)
	code, b := s.e.doWithHeaders("POST", "/v1/regulatory-submissions", "billing", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "kind": "EINVOICE",
		"profile_code": "PEPPOL_INVOICE", "effective_at": "2026-08-15T00:00:00Z", "subject_ref": "INV-2002", "payload_hash": hashA}, map[string]string{"Idempotency-Key": "sub-1"})
	mustStatus(t, 200, code, b)
	assert.Equal(t, subID(a), subID(b))
	code, c := s.e.doWithHeaders("POST", "/v1/regulatory-submissions", "billing", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "kind": "EINVOICE",
		"profile_code": "PEPPOL_INVOICE", "effective_at": "2026-08-15T00:00:00Z", "subject_ref": "INV-OTHER", "payload_hash": hashA}, map[string]string{"Idempotency-Key": "sub-1"})
	mustStatus(t, 409, code, c)

	// The authority reports back.
	ev := func(eid, status, receipt, at string) map[string]any {
		m := map[string]any{"provider_event_id": eid, "status": status, "occurred_at": at}
		if receipt != "" {
			m["receipt_id"] = receipt
		}
		return m
	}
	rp := s.report(t, id, ev("e1", "PROCESSING", "", "2026-08-15T10:00:00Z"))
	assert.Equal(t, true, rp["applied"])
	assert.Equal(t, "PROCESSING", rp["status"])

	// JUR-NEG-09: an acceptance without the authority's receipt is stored but not applied.
	rp = s.report(t, id, ev("e2", "ACCEPTED", "", "2026-08-15T11:00:00Z"))
	assert.Equal(t, false, rp["applied"])
	assert.Equal(t, "MISSING_RECEIPT", rp["disposition"])
	assert.Equal(t, "PROCESSING", rp["status"], "no false accepted state")

	// Out-of-order: an older report arriving after a newer one cannot rewrite history.
	rp = s.report(t, id, ev("e3", "PROCESSING", "", "2026-08-15T09:00:00Z"))
	assert.Equal(t, "STALE", rp["disposition"])

	// JUR-NEG-10: a duplicate callback is stored once and changes nothing.
	rp = s.report(t, id, ev("e1", "PROCESSING", "", "2026-08-15T10:00:00Z"))
	assert.Equal(t, true, rp["replayed"])
	assert.Equal(t, "APPLIED", rp["disposition"], "the original outcome is returned")
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM regulatory_submission_events WHERE submission_id=$1::uuid AND provider_event_id='e1'`, id).Scan(&n))
	assert.Equal(t, 1, n)

	// The receipt arrives.
	rp = s.report(t, id, ev("e4", "ACCEPTED", "AUTH-RCPT-77", "2026-08-15T12:00:00Z"))
	assert.Equal(t, true, rp["applied"])
	assert.Equal(t, "ACCEPTED", rp["status"])
	assert.Equal(t, true, rp["is_terminal"])

	// JUR-NEG-11: a late or contradictory report after the final outcome cannot regress it.
	rp = s.report(t, id, ev("e5", "PROCESSING", "", "2026-08-15T13:00:00Z"))
	assert.Equal(t, "AFTER_TERMINAL", rp["disposition"])
	rp = s.report(t, id, ev("e6", "REJECTED", "", "2026-08-15T14:00:00Z"))
	assert.Equal(t, "AFTER_TERMINAL", rp["disposition"])
	rp = s.report(t, id, ev("e7", "TELEPORTED", "", "2026-08-15T14:00:00Z"))
	assert.Equal(t, "UNKNOWN_STATUS", rp["disposition"])

	code, got := s.e.do("GET", "/v1/regulatory-submissions/"+id, "auditor", nil)
	mustStatus(t, 200, code, got)
	assert.Equal(t, "ACCEPTED", got["submission"].(map[string]any)["status"])
	assert.Len(t, got["events"], 7, "every report, applied or not, is evidence")

	// Database guards: terminal rows never change, evidence is append-only, nothing is deleted.
	_, err := pool.Exec(ctx, `UPDATE regulatory_submissions SET status='REJECTED' WHERE submission_id=$1::uuid`, id)
	require.Error(t, err, "a final outcome cannot be rewritten in SQL")
	_, err = pool.Exec(ctx, `UPDATE regulatory_submission_events SET disposition='APPLIED' WHERE submission_id=$1::uuid`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM regulatory_submission_events WHERE submission_id=$1::uuid`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM regulatory_submissions WHERE submission_id=$1::uuid`, id)
	require.Error(t, err)
	r2 := s.register(t, map[string]any{"subject_ref": "INV-3003"}, 201)
	_, err = pool.Exec(ctx, `UPDATE regulatory_submissions SET payload_hash='sha256:`+strings.Repeat("b", 64)+`' WHERE submission_id=$1::uuid`, subID(r2))
	require.Error(t, err, "only the status of a submission can change")

	// Bad reports and unknown submissions.
	code, _ = s.e.do("POST", "/v1/regulatory-submissions/"+id+"/events", "provider-webhook", map[string]any{"provider_event_id": "x", "status": "processing", "occurred_at": "2026-08-15T10:00:00Z"})
	assert.Equal(t, 400, code)
	code, _ = s.e.do("POST", "/v1/regulatory-submissions/00000000-0000-0000-0000-000000000000/events", "provider-webhook", ev("z", "PROCESSING", "", "2026-08-15T10:00:00Z"))
	assert.Equal(t, 404, code)
	code, _ = s.e.do("GET", "/v1/regulatory-submissions/not-a-uuid", "auditor", nil)
	assert.Equal(t, 404, code)
}

func TestFilings_NeedIndependentApprovalsBeforeSubmission(t *testing.T) {
	s := newSubmissionScenario(t)
	s.release(t)
	filing := func(approvals []map[string]string, want int) map[string]any {
		return s.register(t, map[string]any{"kind": "FILING", "profile_code": "HMRC_VAT_RETURN", "subject_ref": "VAT-2026-Q2",
			"dependencies_used": []map[string]string{{"ref": "hmrc.vat.mtd", "version": "1.0"}}, "approvals": approvals}, want)
	}
	r := filing(nil, 422)
	assert.Equal(t, "APPROVAL_MISSING", r["profile"].(map[string]any)["outcome"])
	// The preparer (the caller, "billing") cannot approve their own filing.
	r = filing([]map[string]string{{"role": "FINANCE_MANAGER", "principal_id": "billing"}, {"role": "TAX_OFFICER", "principal_id": "u3"}}, 422)
	assert.Equal(t, "APPROVAL_MISSING", r["profile"].(map[string]any)["outcome"])
	// One person cannot hold both roles.
	filing([]map[string]string{{"role": "FINANCE_MANAGER", "principal_id": "u2"}, {"role": "TAX_OFFICER", "principal_id": "u2"}}, 422)
	r = filing([]map[string]string{{"role": "FINANCE_MANAGER", "principal_id": "u2"}, {"role": "TAX_OFFICER", "principal_id": "u3"}}, 201)
	assert.Equal(t, "FILING", r["submission"].(map[string]any)["submission_kind"])
	assert.Len(t, r["submission"].(map[string]any)["approvals"], 2)
}

func TestSubmissionProfile_FamilyIsReservedToItsDomain(t *testing.T) {
	s := newSubmissionScenario(t)
	wrong, _, err := st.CreateRule(ctx, domainCreate(s, "TAX", "WRONG_HOME"))
	require.NoError(t, err)
	code, r := s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/provenance", "author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{"parameters": rawMap(t, einvoiceProfile)})
	mustStatus(t, 200, code, r)
	// A profile that smuggles a credential is refused at the edge.
	code, r = s.e.do("PUT", "/v1/admin/rules/"+wrong.JurisdictionRuleID+"/parameters", "rule-author", map[string]any{
		"parameters": rawMap(t, strings.Replace(einvoiceProfile, `"channel":"PEPPOL"`, `"channel":"PEPPOL","api_key":"s3cret"`, 1))})
	assert.Equal(t, 400, code, "%v", r)

	s.packRef = uniqueRef("jur.p")
	code, r = s.e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRef, "pack_name": "P", "owner": "P"})
	mustStatus(t, 201, code, r)
	code, r = s.e.do("POST", "/v1/admin/packs/"+s.packRef+"/versions", "author", manifest(s.packRef, "2026.08.1", s.jur.JurisdictionCode,
		map[string]any{"rule_modules": []string{wrong.JurisdictionRuleID}, "effective_from": "2025-01-01"}))
	mustStatus(t, 201, code, r)
	s.e.do("POST", s.path("/submit-review"), "author", nil)
	code, r = s.e.do("POST", s.path("/compile"), "compiler", nil)
	mustStatus(t, 422, code, r)
	assert.Contains(t, mustString(r["report"]), "JUR-C092")
}
