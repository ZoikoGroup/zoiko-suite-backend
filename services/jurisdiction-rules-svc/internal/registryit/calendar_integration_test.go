//go:build integration

// ZS-JUR-001 Wave 4 (calendar half) end to end: authority calendars and
// obligation rules are authored, sourced and published (by someone else),
// packaged into a signed, tested, certified, released pack, and then served by
// the verified resolver. Due dates are derived, never typed in.
package registryit_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

type dueScenario struct {
	*fixture
	e        *env
	keyRef   string
	calCode  string
	calV1    string // calendar_version_id
	oblID    string
	packRefD string
}

// obligationBody is the VAT-return style rule: end of the next month + 7 days, next business day.
func obligationBody(f *fixture, calCode string) map[string]any {
	return map[string]any{
		"jurisdiction_id": f.jur.JurisdictionID, "obligation_code": "VAT_RETURN", "name": "Quarterly-ish VAT return",
		"period_basis": "MONTHLY", "anchor": "PERIOD_END", "offset_months": 1, "offset_to_month_end": true, "offset_days": 7,
		"business_day_adjustment": "NEXT", "calendar_code": calCode, "effective_from": "2026-01-01",
		"extension_allowed": true, "max_extension_days": 14, "extension_requires_evidence": true,
		"escalation_owner": "tax-ops", "escalation_sla_hours": 24,
	}
}

// newDueScenario authors and publishes one calendar version and one obligation rule.
func newDueScenario(t *testing.T) *dueScenario {
	t.Helper()
	keyRef := "due-key-" + strings.ToLower(uuid.NewString()[:6])
	signer, pubB64 := newSigner(t, keyRef)
	e := newEnvResolver(signer, []string{"RELEASED"}, 0)
	f := newFixture(t, e)
	s := &dueScenario{fixture: f, e: e, keyRef: keyRef, calCode: "cal-" + strings.ToLower(uuid.NewString()[:6])}

	code, r := e.do("POST", "/v1/admin/pack-signing-keys", "security", map[string]any{"key_ref": keyRef, "public_key": pubB64})
	mustStatus(t, 201, code, r)

	code, r = e.do("POST", "/v1/admin/calendars", "cal-author", map[string]any{"calendar_code": s.calCode, "jurisdiction_id": f.jur.JurisdictionID, "authority": "HMRC"})
	mustStatus(t, 201, code, r)
	s.calV1 = s.publishCalendarVersion(t, "2026-01-01", []map[string]string{{"date": "2026-11-09", "name": "Authority closure"}}, 1)

	code, r = e.do("POST", "/v1/admin/obligation-rules", "obl-author", obligationBody(f, s.calCode))
	mustStatus(t, 201, code, r)
	s.oblID = r["obligation_rule_id"].(string)
	assert.EqualValues(t, 1, r["rule_version"])
	code, r = e.do("PUT", "/v1/admin/obligation-rules/"+s.oblID+"/provenance", "obl-author", map[string]any{
		"regime_id": f.regimeID, "interpretation_id": f.interpID, "source_ids": []string{f.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/admin/obligation-rules/"+s.oblID+"/publish", "obl-publisher", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "PUBLISHED", r["status"])
	return s
}

func (s *dueScenario) publishCalendarVersion(t *testing.T, from string, holidays []map[string]string, wantVersion int) string {
	t.Helper()
	code, r := s.e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions", "cal-author", map[string]any{
		"effective_from": from, "timezone": "Europe/London", "weekend_days": []int{0, 6}, "holidays": holidays})
	mustStatus(t, 201, code, r)
	assert.EqualValues(t, wantVersion, r["version"])
	assert.Equal(t, "DRAFT", r["status"])
	id := r["calendar_version_id"].(string)
	v := intStr(wantVersion)
	code, r = s.e.do("PUT", "/v1/admin/calendars/"+s.calCode+"/versions/"+v+"/sources", "cal-author", map[string]any{"source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = s.e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/"+v+"/publish", "cal-publisher", nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, "PUBLISHED", r["status"])
	return id
}

func intStr(n int) string { return strconv.Itoa(n) }

// packWith builds, tests, certifies and releases a pack version carrying the given calendar version.
func (s *dueScenario) packWith(t *testing.T, version, calVersionID, expectedDue string) {
	t.Helper()
	e := s.e
	if s.packRefD == "" {
		s.packRefD = uniqueRef("jur.o")
		code, r := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRefD, "pack_name": "Obligations", "owner": "Tax Operations"})
		mustStatus(t, 201, code, r)
	}
	p := func(suffix string) string { return "/v1/admin/packs/" + s.packRefD + "/versions/" + version + suffix }
	m := manifest(s.packRefD, version, s.jur.JurisdictionCode, map[string]any{
		"calendar_modules": []string{calVersionID}, "obligation_modules": []string{s.oblID}})
	code, r := e.do("POST", "/v1/admin/packs/"+s.packRefD+"/versions", "author", m)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/submit-review"), "author", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", p("/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/sign"), "signer", nil)
	mustStatus(t, 200, code, r)

	jc := s.jur.JurisdictionCode
	mk := func(id, class, jur string, in map[string]any, expect map[string]any) map[string]any {
		base := map[string]any{"jurisdiction": jur, "obligation_code": "VAT_RETURN"}
		for k, v := range in {
			base[k] = v
		}
		return map[string]any{"id": id, "class": class, "input": base, "expect": expect}
	}
	bundle := map[string]any{"bundle_version": "1", "cases": []map[string]any{
		mk("g1", "GOLDEN", jc, map[string]any{"period_end": "2026-08-31"}, map[string]any{"outcome": "DUE_DATE_CALCULATED", "due_date": "2026-10-07", "obligation_rule_id": s.oblID}),
		mk("b1", "BOUNDARY", jc, map[string]any{"period_end": "2026-09-30"}, map[string]any{"outcome": "DUE_DATE_CALCULATED", "due_date": expectedDue}),
		mk("n1", "NEGATIVE", jc, map[string]any{"period_end": "2026-09-30", "extension_days": 15, "extension_evidence_ref": "E-1"}, map[string]any{"outcome": "EXTENSION_NOT_PERMITTED"}),
		mk("n2", "NEGATIVE", "ATLANTIS", map[string]any{"period_end": "2026-09-30"}, map[string]any{"outcome": "UNSUPPORTED_JURISDICTION"}),
	}}
	code, r = e.do("POST", p("/test-bundle"), "tester", bundle)
	require.Contains(t, []int{200, 201}, code, "%v", r)
	code, r = e.do("POST", p("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	require.Equal(t, true, r["passed"], "%v", r["result"])
	code, r = e.do("POST", p("/reviews"), "reviewer-"+version, map[string]any{"role": "TAX", "decision": "APPROVE"})
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/certify"), "certifier", nil)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/publish"), "releaser", nil)
	mustStatus(t, 200, code, r)
}

func (s *dueScenario) calc(t *testing.T, body map[string]any, want int) map[string]any {
	t.Helper()
	base := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "obligation_code": "VAT_RETURN"}
	for k, v := range body {
		base[k] = v
	}
	code, r := s.e.do("POST", "/v1/obligation-calculations:calculate", "tax-engine", base)
	require.Equal(t, want, code, "%v", r)
	return r
}

func calcOf(r map[string]any) map[string]any {
	c, _ := r["calculation"].(map[string]any)
	return c
}

func dueOf(r map[string]any) map[string]any {
	d, _ := calcOf(r)["calculation"].(map[string]any)
	return d
}

func TestCalendarAndObligation_AuthoringIsSourcedIndependentAndImmutable(t *testing.T) {
	s := newDueScenario(t)
	e := s.e

	// The fixture rule is PUBLISHED: nothing about it or the calendar can change any more.
	code, r := e.do("PUT", "/v1/admin/obligation-rules/"+s.oblID+"/provenance", "obl-author", map[string]any{"regime_id": s.regimeID})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "not_draft", r["error"])
	code, r = e.do("PUT", "/v1/admin/calendars/"+s.calCode+"/versions/1/sources", "cal-author", map[string]any{"source_ids": []string{s.sourceID}})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "not_draft", r["error"])
	for _, q := range []string{
		`UPDATE regulatory_calendar_versions SET timezone='UTC' WHERE calendar_version_id=$1`,
		`UPDATE regulatory_holidays SET name='x' WHERE calendar_version_id=$1`,
		`DELETE FROM regulatory_holidays WHERE calendar_version_id=$1`,
		`INSERT INTO regulatory_holidays VALUES ($1,'2026-12-25','Christmas')`,
		`DELETE FROM calendar_version_sources WHERE calendar_version_id=$1`,
		`DELETE FROM regulatory_calendar_versions WHERE calendar_version_id=$1`,
	} {
		_, err := pool.Exec(ctx, q, s.calV1)
		require.Error(t, err, q)
	}
	for _, q := range []string{
		`UPDATE obligation_rules SET offset_days=0 WHERE obligation_rule_id=$1`,
		`DELETE FROM obligation_rule_sources WHERE obligation_rule_id=$1`,
		`DELETE FROM obligation_rules WHERE obligation_rule_id=$1`,
	} {
		_, err := pool.Exec(ctx, q, s.oblID)
		require.Error(t, err, q)
	}

	// Independence: the author cannot publish their own work, even straight in SQL.
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions", "cal-author", map[string]any{
		"effective_from": "2027-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6}, "holidays": []map[string]string{}})
	mustStatus(t, 201, code, r)
	v2 := r["calendar_version_id"].(string)
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/2/publish", "cal-publisher", nil)
	mustStatus(t, 409, code, r) // no sources yet
	assert.Equal(t, "source_not_ready", r["error"])
	code, r = e.do("PUT", "/v1/admin/calendars/"+s.calCode+"/versions/2/sources", "cal-author", map[string]any{"source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/2/publish", "cal-author", nil)
	mustStatus(t, 403, code, r)
	assert.Equal(t, "segregation_of_duties", r["error"])
	_, err := pool.Exec(ctx, `UPDATE regulatory_calendar_versions SET status='PUBLISHED', published_at=NOW(), published_by=created_by_principal_id WHERE calendar_version_id=$1`, v2)
	require.Error(t, err, "published_by must differ from the author at the database too")
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/2/publish", "cal-publisher", nil)
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/2/publish", "someone", nil)
	mustStatus(t, 200, code, r) // already published: a no-op
	assert.Equal(t, 1, e.pub.count(events.EventCalendarChanged)-1, "publishing emits jurisdiction.calendar.changed once per version (v1 earlier + v2 here)")

	// Obligation rule publishing needs provenance and a different principal.
	code, r = e.do("POST", "/v1/admin/obligation-rules", "obl-author", obligationBody(s.fixture, s.calCode))
	mustStatus(t, 201, code, r)
	assert.EqualValues(t, 2, r["rule_version"], "versions are assigned by the server per jurisdiction and code")
	id2 := r["obligation_rule_id"].(string)
	code, r = e.do("POST", "/v1/admin/obligation-rules/"+id2+"/publish", "obl-publisher", nil)
	mustStatus(t, 409, code, r) // no provenance
	assert.Equal(t, "source_not_ready", r["error"])
	_, pending := e.do("POST", "/v1/admin/interpretations", "author", map[string]any{"jurisdiction_id": s.jur.JurisdictionID, "subject": "s", "decision": "d", "rationale": "r", "source_ids": []string{s.sourceID}})
	code, r = e.do("PUT", "/v1/admin/obligation-rules/"+id2+"/provenance", "obl-author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": pending["interpretation_id"], "source_ids": []string{s.sourceID}})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "interpretation_not_approved", r["error"], "JUR-NEG-18")
	code, r = e.do("PUT", "/v1/admin/obligation-rules/"+id2+"/provenance", "obl-author", map[string]any{
		"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	mustStatus(t, 200, code, r)
	code, r = e.do("POST", "/v1/admin/obligation-rules/"+id2+"/publish", "obl-author", nil)
	mustStatus(t, 403, code, r)
	code, r = e.do("POST", "/v1/admin/obligation-rules/"+id2+"/publish", "obl-publisher", nil)
	mustStatus(t, 200, code, r)

	// An unknown calendar cannot be named by a published rule.
	bad := obligationBody(s.fixture, "no-such-calendar")
	code, r = e.do("POST", "/v1/admin/obligation-rules", "obl-author", bad)
	mustStatus(t, 201, code, r)
	id3 := r["obligation_rule_id"].(string)
	e.do("PUT", "/v1/admin/obligation-rules/"+id3+"/provenance", "obl-author", map[string]any{"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	code, r = e.do("POST", "/v1/admin/obligation-rules/"+id3+"/publish", "obl-publisher", nil)
	mustStatus(t, 400, code, r)
	assert.Equal(t, "invalid_reference", r["error"])

	// A source nobody has reviewed cannot back a publication.
	_, fresh := e.do("POST", "/v1/admin/sources", "author", sourceBody(s.jur.JurisdictionID))
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions", "cal-author", map[string]any{
		"effective_from": "2028-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6}, "holidays": []map[string]string{}})
	mustStatus(t, 201, code, r)
	e.do("PUT", "/v1/admin/calendars/"+s.calCode+"/versions/3/sources", "cal-author", map[string]any{"source_ids": []string{fresh["source_id"].(string)}})
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/3/publish", "cal-publisher", nil)
	mustStatus(t, 409, code, r)
	assert.Equal(t, "source_not_ready", r["error"])

	// Reads.
	code, got := e.do("GET", "/v1/calendars/"+s.calCode+"/versions/1", "", nil)
	mustStatus(t, 200, code, got)
	assert.Equal(t, "2026-11-09", got["holidays"].([]any)[0].(map[string]any)["date"])
	code, list := e.do("GET", "/v1/calendars/"+s.calCode+"/versions", "", nil)
	mustStatus(t, 200, code, list)
	assert.Len(t, list["versions"], 3)
	code, _ = e.do("GET", "/v1/calendars/nope", "", nil)
	assert.Equal(t, 404, code)
	code, _ = e.do("GET", "/v1/calendars/"+s.calCode+"/versions/abc", "", nil)
	assert.Equal(t, 400, code)
	code, ol := e.do("GET", "/v1/obligation-rules?obligation_code=VAT_RETURN&jurisdiction_id="+s.jur.JurisdictionID, "", nil)
	mustStatus(t, 200, code, ol)
	assert.GreaterOrEqual(t, len(ol["obligation_rules"].([]any)), 2)
	code, _ = e.do("GET", "/v1/obligation-rules/"+uuid.NewString(), "", nil)
	assert.Equal(t, 404, code)
}

func TestCalendarAndObligation_RejectMalformedDefinitions(t *testing.T) {
	s := newDueScenario(t)
	e := s.e
	cv := func(m map[string]any) (int, map[string]any) {
		base := map[string]any{"effective_from": "2029-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6}, "holidays": []map[string]string{}}
		for k, v := range m {
			base[k] = v
		}
		return e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions", "cal-author", base)
	}
	for name, m := range map[string]map[string]any{
		"unknown zone":  {"timezone": "Mars/Olympus"},
		"weekend 7":     {"weekend_days": []int{7}},
		"bad cutoff":    {"cutoff_time": "25:61"},
		"duplicate hol": {"holidays": []map[string]string{{"date": "2029-01-02", "name": "a"}, {"date": "2029-01-02", "name": "b"}}},
		"bad date":      {"effective_from": "01/01/2029"},
	} {
		code, r := cv(m)
		assert.Equal(t, 400, code, "%s: %v", name, r)
	}
	code, r := e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions", "cal-author", map[string]any{"effective_from": "2029-01-01", "timezone": "Europe/London", "holidays": []map[string]string{}})
	mustStatus(t, 400, code, r) // weekend_days is required, not defaulted
	code, _ = e.do("POST", "/v1/admin/calendars/nope-cal/versions", "cal-author", map[string]any{"effective_from": "2029-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6}})
	assert.Equal(t, 404, code)
	code, _ = e.do("POST", "/v1/admin/calendars", "cal-author", map[string]any{"calendar_code": "Bad Code", "jurisdiction_id": s.jur.JurisdictionID, "authority": "x"})
	assert.Equal(t, 400, code)
	code, _ = e.do("POST", "/v1/admin/calendars", "cal-author", map[string]any{"calendar_code": s.calCode, "jurisdiction_id": s.jur.JurisdictionID, "authority": "OTHER AUTHORITY"})
	assert.Equal(t, 409, code, "the same code with a different authority is a conflict")
	code, r = e.do("POST", "/v1/admin/calendars", "cal-author", map[string]any{"calendar_code": s.calCode, "jurisdiction_id": s.jur.JurisdictionID, "authority": "HMRC"})
	mustStatus(t, 200, code, r)

	for name, m := range map[string]map[string]any{
		"bad code":        {"obligation_code": "vat return"},
		"bad basis":       {"period_basis": "WEEKLY"},
		"bad anchor":      {"anchor": "TODAY"},
		"negative offset": {"offset_days": -1},
		"adjust no cal":   {"calendar_code": ""},
		"ext no cap":      {"max_extension_days": 0},
		"month end zero":  {"offset_months": 0},
		"bad window":      {"effective_to": "2025-01-01"},
	} {
		body := obligationBody(s.fixture, s.calCode)
		for k, v := range m {
			body[k] = v
		}
		code, r := e.do("POST", "/v1/admin/obligation-rules", "obl-author", body)
		assert.Equal(t, 400, code, "%s: %v", name, r)
	}
	code, _ = e.do("POST", "/v1/admin/obligation-rules", "obl-author", map[string]any{"jurisdiction_id": uuid.NewString(), "obligation_code": "VAT_RETURN", "name": "n",
		"period_basis": "MONTHLY", "anchor": "PERIOD_END", "effective_from": "2026-01-01"})
	assert.Equal(t, 404, code, "unknown jurisdiction")
	code, _ = e.do("POST", "/v1/admin/calendars", "", map[string]any{})
	assert.Equal(t, 401, code)
	e.auth.deny = "regulatory_calendar_version.publish"
	code, _ = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions/1/publish", "cal-publisher", nil)
	assert.Equal(t, 403, code)
}

func TestDueDate_FromAReleasedPack_IsDerivedExplainedAndRecorded(t *testing.T) {
	s := newDueScenario(t)
	s.packWith(t, "2026.08.1", s.calV1, "2026-11-10")

	// 30 Sep 2026: end of next month (31 Oct) + 7 = Sat 7 Nov -> Mon 9 Nov is a listed holiday -> Tue 10 Nov.
	r := s.calc(t, map[string]any{"period_end": "2026-09-30"}, 200)
	c := calcOf(r)
	require.Equal(t, "DUE_DATE_CALCULATED", c["outcome"])
	due := dueOf(r)
	assert.Equal(t, "2026-11-10", due["due_date"])
	assert.Equal(t, "2026-11-07", due["unadjusted_date"])
	assert.Equal(t, true, due["adjusted"])
	assert.EqualValues(t, 1, due["calendar_version"])
	assert.Equal(t, "Europe/London", due["timezone"])
	assert.Equal(t, "tax-ops", due["escalation_owner"])
	assert.Contains(t, mustString(due["explanation"]), "Authority closure")
	pack := c["pack"].(map[string]any)
	assert.Equal(t, s.packRefD, pack["pack_ref"])
	assert.NotEmpty(t, pack["certification_id"])
	assert.Len(t, c["sources"], 1)
	id := r["decision_id"].(string)

	// Evidence: the exact pack release and obligation rule are on the ledger, and immutable.
	var kind, artDigest, ruleID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT decision_kind, artifact_digest, rule_id::text FROM rule_decision_evidence WHERE decision_id=$1`, id).Scan(&kind, &artDigest, &ruleID))
	assert.Equal(t, "OBLIGATION", kind)
	assert.Equal(t, pack["artifact_digest"], artDigest)
	assert.Equal(t, s.oblID, ruleID)
	code, got := s.e.do("GET", "/v1/rule-decisions/"+id, "auditor", nil)
	mustStatus(t, 200, code, got)
	_, err := pool.Exec(ctx, `UPDATE rule_decision_evidence SET outcome='NO_OBLIGATION_RULE' WHERE decision_id=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO rule_decision_evidence (requested_by, request_digest, request, effective_at, outcome, response, decision_kind)
		VALUES ('x','sha256:`+strings.Repeat("0", 64)+`','{}'::jsonb,NOW(),'DUE_DATE_CALCULATED','{}'::jsonb,'OBLIGATION')`)
	require.Error(t, err, "a calculated due date cannot be stored without its pack and rule basis")

	// A business day is untouched; the instant is the end of the authority-local day.
	r = s.calc(t, map[string]any{"period_end": "2026-08-31"}, 200)
	assert.Equal(t, "2026-10-07", dueOf(r)["due_date"])
	assert.Equal(t, false, dueOf(r)["adjusted"])

	// Overdue verdict against the due instant (end of 10 Nov in London = GMT).
	r = s.calc(t, map[string]any{"period_end": "2026-09-30", "as_of": "2026-11-10T12:00:00Z"}, 200)
	assert.Equal(t, false, r["overdue"])
	r = s.calc(t, map[string]any{"period_end": "2026-09-30", "as_of": "2026-11-11T00:00:01Z"}, 200)
	assert.Equal(t, true, r["overdue"])

	// Evidenced extension within the cap is applied before the business-day adjustment.
	r = s.calc(t, map[string]any{"period_end": "2026-09-30", "extension_days": 5, "extension_evidence_ref": "EXT-77"}, 200)
	assert.Equal(t, "2026-11-12", dueOf(r)["due_date"])
	// Refusals are explicit and recorded.
	r = s.calc(t, map[string]any{"period_end": "2026-09-30", "extension_days": 15, "extension_evidence_ref": "EXT-78"}, 422)
	assert.Equal(t, "EXTENSION_NOT_PERMITTED", calcOf(r)["outcome"])
	r = s.calc(t, map[string]any{"period_end": "2026-09-30", "extension_days": 5}, 422)
	assert.Contains(t, mustString(calcOf(r)["explanation"]), "evidenced")
	r = s.calc(t, map[string]any{"period_end": "2026-09-15"}, 422)
	assert.Equal(t, "INVALID_FACTS", calcOf(r)["outcome"], "a MONTHLY period must end on a month end")
	r = s.calc(t, map[string]any{"jurisdiction": "ATLANTIS", "period_end": "2026-09-30"}, 422)
	assert.Equal(t, "UNSUPPORTED_JURISDICTION", calcOf(r)["outcome"])
	r = s.calc(t, map[string]any{"obligation_code": "OTHER_FILING", "period_end": "2026-09-30"}, 200)
	assert.Equal(t, "NO_OBLIGATION_RULE", calcOf(r)["outcome"])
	r = s.calc(t, map[string]any{"period_end": "2025-12-31"}, 200)
	assert.Equal(t, "NO_OBLIGATION_RULE", calcOf(r)["outcome"], "before the rule is effective: nothing is invented")

	// Request hygiene.
	code, _ = s.e.do("POST", "/v1/obligation-calculations:calculate", "tax-engine", map[string]any{"jurisdiction": s.jur.JurisdictionCode, "obligation_code": "VAT_RETURN"})
	assert.Equal(t, 400, code, "no anchor fact at all")
	code, _ = s.e.do("POST", "/v1/obligation-calculations:calculate", "", map[string]any{})
	assert.Equal(t, 401, code)
	s.e.auth.deny = "obligation_calculation.calculate"
	code, _ = s.e.do("POST", "/v1/obligation-calculations:calculate", "tax-engine", map[string]any{"jurisdiction": "x", "obligation_code": "Y"})
	assert.Equal(t, 403, code)
	s.e.auth.deny = ""

	// Idempotency binds a key to one question.
	do := func(key string, extra map[string]any) (int, map[string]any) {
		body := map[string]any{"jurisdiction": s.jur.JurisdictionCode, "obligation_code": "VAT_RETURN", "period_end": "2026-09-30"}
		for k, v := range extra {
			body[k] = v
		}
		return s.e.doWithHeaders("POST", "/v1/obligation-calculations:calculate", "tax-engine", body, map[string]string{"Idempotency-Key": key})
	}
	code, a := do("due-1", nil)
	mustStatus(t, 200, code, a)
	code, b := do("due-1", map[string]any{"as_of": "2027-01-01T00:00:00Z"})
	mustStatus(t, 200, code, b)
	assert.Equal(t, a["decision_id"], b["decision_id"], "as_of only asks for a verdict; it is the same question")
	assert.Equal(t, true, b["replayed"])
	code, c2 := do("due-1", map[string]any{"period_end": "2026-08-31"})
	mustStatus(t, 409, code, c2)
}

func TestHolidayAmendment_ChangesOnlyNewCalculations_AndPastOnesStayReproducible(t *testing.T) {
	s := newDueScenario(t)
	s.packWith(t, "2026.08.1", s.calV1, "2026-11-10")
	first := s.calc(t, map[string]any{"period_end": "2026-09-30"}, 200)
	require.Equal(t, "2026-11-10", dueOf(first)["due_date"])

	// JUR-NEG-13: the authority adds 10 Nov as a holiday. That is a NEW calendar version, in a NEW pack version.
	v2 := s.publishCalendarVersion(t, "2026-01-01", []map[string]string{
		{"date": "2026-11-09", "name": "Authority closure"}, {"date": "2026-11-10", "name": "Added by amendment"}}, 2)
	s.packWith(t, "2026.09.1", v2, "2026-11-11")

	now := s.calc(t, map[string]any{"period_end": "2026-09-30"}, 200)
	assert.Equal(t, "2026-11-11", dueOf(now)["due_date"], "new calculations use the amended calendar")
	assert.EqualValues(t, 2, dueOf(now)["calendar_version"])
	assert.Equal(t, "2026.09.1", calcOf(now)["pack"].(map[string]any)["version"])

	// The old decision is untouched, and replaying it against its own pack reproduces it exactly.
	var stillOld string
	require.NoError(t, pool.QueryRow(ctx, `SELECT response->'calculation'->>'due_date' FROM rule_decision_evidence WHERE decision_id=$1`, first["decision_id"]).Scan(&stillOld))
	assert.Equal(t, "2026-11-10", stillOld)
	replay := s.calc(t, map[string]any{"period_end": "2026-09-30", "pack_ref": s.packRefD, "pack_version": "2026.08.1"}, 200)
	assert.Equal(t, "2026-11-10", dueOf(replay)["due_date"])
	assert.EqualValues(t, 1, dueOf(replay)["calendar_version"])

	// A period the amendment does not touch gives the same answer in both.
	a := s.calc(t, map[string]any{"period_end": "2026-08-31"}, 200)
	assert.Equal(t, "2026-10-07", dueOf(a)["due_date"])
}

func TestCompile_CalendarAndObligationModuleRules(t *testing.T) {
	s := newDueScenario(t)
	e := s.e
	ref := uniqueRef("jur.q")
	code, r := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": ref, "pack_name": "Q", "owner": "T"})
	mustStatus(t, 201, code, r)
	draft := func(version string, extra map[string]any) {
		code, r := e.do("POST", "/v1/admin/packs/"+ref+"/versions", "author", manifest(ref, version, s.jur.JurisdictionCode, extra))
		mustStatus(t, 201, code, r)
		code, r = e.do("POST", "/v1/admin/packs/"+ref+"/versions/"+version+"/submit-review", "author", nil)
		mustStatus(t, 200, code, r)
	}
	findings := func(version string) string {
		code, r := e.do("POST", "/v1/admin/packs/"+ref+"/versions/"+version+"/compile", "compiler", nil)
		mustStatus(t, 422, code, r)
		return mustString(r["report"])
	}

	// An obligation that names a calendar the pack does not carry (the artifact must be self-contained).
	draft("2026.01.1", map[string]any{"obligation_modules": []string{s.oblID}})
	assert.Contains(t, findings("2026.01.1"), "JUR-C087")

	// A calendar version that is still DRAFT cannot be packaged.
	code, r = e.do("POST", "/v1/admin/calendars/"+s.calCode+"/versions", "cal-author", map[string]any{
		"effective_from": "2030-01-01", "timezone": "Europe/London", "weekend_days": []int{0, 6}, "holidays": []map[string]string{}})
	mustStatus(t, 201, code, r)
	draftCal := r["calendar_version_id"].(string)
	draft("2026.02.1", map[string]any{"obligation_modules": []string{s.oblID}, "calendar_modules": []string{s.calV1, draftCal}})
	assert.Contains(t, findings("2026.02.1"), "JUR-C071")

	// Unknown module ids are refused when the version is drafted.
	code, r = e.do("POST", "/v1/admin/packs/"+ref+"/versions", "author", manifest(ref, "2026.03.1", s.jur.JurisdictionCode, map[string]any{"calendar_modules": []string{uuid.NewString()}}))
	mustStatus(t, 400, code, r)
	code, r = e.do("POST", "/v1/admin/packs/"+ref+"/versions", "author", manifest(ref, "2026.04.1", s.jur.JurisdictionCode, map[string]any{"obligation_modules": []string{"not-a-uuid"}}))
	mustStatus(t, 400, code, r)

	// Overlapping obligation rules of the same code are an unresolvable conflict.
	code, r = e.do("POST", "/v1/admin/obligation-rules", "obl-author", obligationBody(s.fixture, s.calCode))
	mustStatus(t, 201, code, r)
	dup := r["obligation_rule_id"].(string)
	e.do("PUT", "/v1/admin/obligation-rules/"+dup+"/provenance", "obl-author", map[string]any{"regime_id": s.regimeID, "interpretation_id": s.interpID, "source_ids": []string{s.sourceID}})
	code, r = e.do("POST", "/v1/admin/obligation-rules/"+dup+"/publish", "obl-publisher", nil)
	mustStatus(t, 200, code, r)
	draft("2026.05.1", map[string]any{"obligation_modules": []string{s.oblID, dup}, "calendar_modules": []string{s.calV1}})
	assert.Contains(t, findings("2026.05.1"), "JUR-C088")
}

func TestHarness_ObligationCoverageGapsBlockCertification(t *testing.T) {
	s := newDueScenario(t)
	e := s.e
	s.packRefD = uniqueRef("jur.h")
	code, r := e.do("POST", "/v1/admin/packs", "author", map[string]any{"pack_ref": s.packRefD, "pack_name": "H", "owner": "T"})
	mustStatus(t, 201, code, r)
	p := func(suffix string) string { return "/v1/admin/packs/" + s.packRefD + "/versions/2026.08.1" + suffix }
	code, r = e.do("POST", "/v1/admin/packs/"+s.packRefD+"/versions", "author", manifest(s.packRefD, "2026.08.1", s.jur.JurisdictionCode,
		map[string]any{"calendar_modules": []string{s.calV1}, "obligation_modules": []string{s.oblID}}))
	mustStatus(t, 201, code, r)
	e.do("POST", p("/submit-review"), "author", nil)
	code, r = e.do("POST", p("/compile"), "compiler", nil)
	mustStatus(t, 201, code, r)
	e.do("POST", p("/sign"), "signer", nil)

	// Every case passes, but the rule adjusts to a business day and permits extensions: both must be demonstrated.
	only := map[string]any{"bundle_version": "1", "cases": []map[string]any{
		{"id": "g1", "class": "GOLDEN", "input": map[string]any{"jurisdiction": s.jur.JurisdictionCode, "obligation_code": "VAT_RETURN", "period_end": "2026-08-31"},
			"expect": map[string]any{"outcome": "DUE_DATE_CALCULATED", "due_date": "2026-10-07"}},
		{"id": "n2", "class": "NEGATIVE", "input": map[string]any{"jurisdiction": "ATLANTIS", "obligation_code": "VAT_RETURN", "period_end": "2026-09-30"},
			"expect": map[string]any{"outcome": "UNSUPPORTED_JURISDICTION"}}}}
	code, r = e.do("POST", p("/test-bundle"), "tester", only)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	assert.Equal(t, false, r["passed"])
	gaps := mustString(r["result"].(map[string]any)["coverage_gaps"])
	assert.Contains(t, gaps, "JUR-T011")
	assert.Contains(t, gaps, "JUR-T012")
	e.do("POST", p("/reviews"), "reviewer-x", map[string]any{"role": "TAX", "decision": "APPROVE"})
	code, r = e.do("POST", p("/certify"), "certifier", nil)
	mustStatus(t, 409, code, r)
	assert.Contains(t, reasonsOf(r), "tests_failed")

	// A wrong expected date is a failing case, not a coverage gap.
	wrong := map[string]any{"bundle_version": "1", "cases": append(only["cases"].([]map[string]any), map[string]any{
		"id": "b1", "class": "BOUNDARY", "input": map[string]any{"jurisdiction": s.jur.JurisdictionCode, "obligation_code": "VAT_RETURN", "period_end": "2026-09-30"},
		"expect": map[string]any{"outcome": "DUE_DATE_CALCULATED", "due_date": "2026-11-09"}})}
	code, r = e.do("POST", p("/test-bundle"), "tester", wrong)
	mustStatus(t, 201, code, r)
	code, r = e.do("POST", p("/test-runs"), "tester", nil)
	mustStatus(t, 201, code, r)
	assert.EqualValues(t, 1, r["result"].(map[string]any)["failed"], "the harness caught a due date computed wrongly by the test author (9 Nov is a holiday)")
	_ = domain.DueCalculated
}
