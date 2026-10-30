//go:build integration

// ZS-JUR-001 s23, s31 and JUR-NEG-19 end to end: withdrawing a version opens a remediation case for the decisions already made with it; each decision needs
// a disposition with evidence; the case closes only when every one has one and only by someone other than its opener. The decisions are never changed.
package registryit_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/jurisdiction-rules-svc/internal/events"
)

// decideN resolves n times and returns the decision ids, each one a recorded outcome made with the pack version in force.
func decideN(t *testing.T, c *certFixture, n int) []string {
	t.Helper()
	var ids []string
	for i := 0; i < n; i++ {
		code, r := c.e.do("POST", "/v1/rule-resolutions:resolve", "tax-engine", c.resolveReq(time.Now()))
		mustStatus(t, 200, code, r)
		ids = append(ids, r["decision_id"].(string))
	}
	return ids
}

func disp(decision, kind, evidence string, newDecision string) map[string]any {
	m := map[string]any{"decision_id": decision, "disposition": kind, "evidence_ref": evidence}
	if newDecision != "" {
		m["new_decision_id"] = newDecision
	}
	return m
}

func TestRemediation_WithdrawalOpensACaseThatCannotCloseUntilEveryDecisionIsAnsweredFor(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e

	// Three outcomes are produced with 2026.08.1. Then a corrected version is released, and a fourth outcome is produced with it.
	old := decideN(t, c, 3)
	c.certifyVersion(t, "2026.09.1")
	setStatus(t, c, "2026.09.1", "RELEASED")
	fresh := decideN(t, c, 1)[0]
	var freshVersion string
	require.NoError(t, pool.QueryRow(ctx, `SELECT pack_version FROM rule_decision_evidence WHERE decision_id=$1`, fresh).Scan(&freshVersion))
	require.Equal(t, "2026.09.1", freshVersion, "the replacement decision was made with the corrected version")

	// Withdrawing 2026.08.1 changes future resolution only; the decisions already made are tracked, in the same transaction.
	code, r := e.do("POST", c.fixture.path("2026.08.1", "/withdraw"), "releaser", map[string]any{"reason": "wrong rate", "evidence_ref": "INC-1"})
	mustStatus(t, 200, code, r)
	caseID, _ := r["remediation_case_id"].(string)
	require.NotEmpty(t, caseID, "a withdrawal with decisions in its history opens a case")
	assert.Equal(t, true, r["remediation_opened"])
	assert.EqualValues(t, 3, r["remediation_decisions"], "only the decisions made with the withdrawn version, not the corrected one")
	assert.Equal(t, 1, e.pub.count(events.EventRemediationOpened))

	code, cs := e.do("GET", "/v1/admin/remediation-cases/"+caseID, "auditor", nil)
	mustStatus(t, 200, code, cs)
	assert.Equal(t, "WITHDRAW", cs["trigger"])
	assert.Equal(t, "OPEN", cs["status"])
	assert.Equal(t, "2026.08.1", cs["version"])
	assert.Nil(t, cs["ring_code"], "a withdrawal covers every ring and region")
	assert.Equal(t, "releaser", cs["opened_by"])
	prog := cs["progress"].(map[string]any)
	assert.EqualValues(t, 3, prog["total"])
	assert.EqualValues(t, 3, prog["pending"])

	code, lst := e.do("GET", "/v1/admin/remediation-cases/"+caseID+"/decisions?pending=true", "auditor", nil)
	mustStatus(t, 200, code, lst)
	assert.Len(t, lst["decisions"], 3)
	code, lst = e.do("GET", "/v1/admin/remediation-cases?status=OPEN", "auditor", nil)
	mustStatus(t, 200, code, lst)
	assert.NotEmpty(t, lst["cases"])
	code, _ = e.do("GET", "/v1/admin/remediation-cases?status=PENDING", "auditor", nil)
	assert.Equal(t, 400, code)
	code, _ = e.do("GET", "/v1/admin/remediation-cases/"+uuid.NewString(), "auditor", nil)
	assert.Equal(t, 404, code)
	code, _ = e.do("GET", "/v1/admin/remediation-cases/not-a-uuid", "auditor", nil)
	assert.Equal(t, 404, code)

	// It cannot close while any decision is unanswered.
	code, r = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/close", "reviewer-2", map[string]any{"note": "done"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, mustString(r["reasons"]), "3 of 3 decision(s) have no disposition")
	code, _ = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/close", "reviewer-2", map[string]any{})
	assert.Equal(t, 400, code, "a closure note is required")

	// A disposition must be well formed.
	post := func(body ...map[string]any) (int, map[string]any) {
		return e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/dispositions", "tax-analyst", map[string]any{"dispositions": body})
	}
	for name, d := range map[string]map[string]any{
		"unknown disposition": disp(old[0], "FORGOTTEN", "E-1", ""),
		"no evidence":         disp(old[0], "RECONFIRMED", "", ""),
		"recalc needs new":    disp(old[0], "RECALCULATED", "E-1", ""),
		"reconfirmed no new":  disp(old[0], "RECONFIRMED", "E-1", fresh),
	} {
		code, r = post(d)
		assert.Equal(t, 400, code, "%s: %v", name, r)
	}
	code, _ = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/dispositions", "tax-analyst", map[string]any{"dispositions": []any{}})
	assert.Equal(t, 400, code)
	code, _ = post(disp(old[0], "RECONFIRMED", "E-1", ""), disp(old[0], "NOT_AFFECTED", "E-2", ""))
	assert.Equal(t, 400, code, "the same decision twice in one batch")

	// A decision that is not part of the case is refused, and so is a replacement made with the very version under remediation.
	code, r = post(disp(uuid.NewString(), "RECONFIRMED", "E-1", ""))
	mustStatus(t, 422, code, r)
	assert.Equal(t, "decision_not_in_case", r["error"])
	code, r = post(disp(old[0], "RECALCULATED", "E-1", old[1]))
	mustStatus(t, 422, code, r)
	assert.Equal(t, "remediation_refused", r["error"])
	assert.Contains(t, mustString(r["detail"]), "same pack version")

	// Record the dispositions. A repeat is a replay; a different answer for the same decision is refused (dispositions are final).
	code, r = post(disp(old[0], "RECALCULATED", "RECALC-77", fresh), disp(old[1], "RECONFIRMED", "REV-3", ""))
	mustStatus(t, 200, code, r)
	assert.EqualValues(t, 2, r["recorded"])
	code, r = post(disp(old[0], "RECALCULATED", "RECALC-77", fresh))
	mustStatus(t, 200, code, r)
	assert.EqualValues(t, 1, r["replayed"])
	assert.EqualValues(t, 0, r["recorded"])
	code, r = post(disp(old[0], "RECONFIRMED", "REV-4", ""))
	mustStatus(t, 409, code, r)
	assert.Equal(t, "disposition_differs", r["error"])
	code, cs = e.do("GET", "/v1/admin/remediation-cases/"+caseID, "auditor", nil)
	mustStatus(t, 200, code, cs)
	assert.EqualValues(t, 1, cs["progress"].(map[string]any)["pending"])
	assert.EqualValues(t, 1, cs["progress"].(map[string]any)["by_disposition"].(map[string]any)["RECALCULATED"])
	code, lst = e.do("GET", "/v1/admin/remediation-cases/"+caseID+"/decisions?pending=true", "auditor", nil)
	mustStatus(t, 200, code, lst)
	require.Len(t, lst["decisions"], 1)
	assert.Equal(t, old[2], lst["decisions"].([]any)[0].(map[string]any)["decision_id"], "only the unanswered decision remains pending")

	// One decision is still pending, so it cannot close yet.
	code, r = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/close", "reviewer-2", map[string]any{"note": "almost"})
	mustStatus(t, 409, code, r)
	assert.Contains(t, mustString(r["reasons"]), "1 of 3 decision(s) have no disposition")
	code, r = post(disp(old[2], "NOT_AFFECTED", "REV-5", ""))
	mustStatus(t, 200, code, r)

	// All answered, but the person who opened the case cannot close it.
	code, r = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/close", "releaser", map[string]any{"note": "all done"})
	mustStatus(t, 403, code, r)
	assert.Equal(t, "OPEN", versionCaseStatus(t, caseID))
	code, r = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/close", "reviewer-2", map[string]any{"note": "every outcome re-checked against the corrected version"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, true, r["changed"])
	assert.Equal(t, "CLOSED", r["case"].(map[string]any)["status"])
	assert.Equal(t, "reviewer-2", r["case"].(map[string]any)["closed_by"])
	assert.Equal(t, 1, e.pub.count(events.EventRemediationClosed))

	// Closing again changes nothing and is not announced twice; a closed case takes no more dispositions.
	code, r = e.do("POST", "/v1/admin/remediation-cases/"+caseID+"/close", "reviewer-3", map[string]any{"note": "again"})
	mustStatus(t, 200, code, r)
	assert.Equal(t, false, r["changed"])
	assert.Equal(t, 1, e.pub.count(events.EventRemediationClosed))
	code, r = post(disp(old[0], "RECONFIRMED", "REV-9", ""))
	mustStatus(t, 409, code, r)

	// The decisions themselves are untouched: remediation records answers, it never rewrites the outcomes.
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM rule_decision_evidence WHERE decision_id = ANY($1::uuid[]) AND pack_version='2026.08.1'`, old).Scan(&n))
	assert.Equal(t, 3, n)
}

func versionCaseStatus(t *testing.T, caseID string) string {
	t.Helper()
	var s string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM remediation_cases WHERE case_id::text=$1`, caseID).Scan(&s))
	return s
}

func TestRemediation_ManualCaseAndNothingToRemediate(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e
	decideN(t, c, 2)
	c.certifyVersion(t, "2026.09.1") // never used: no decisions were made with it

	open := func(body map[string]any) (int, map[string]any) {
		base := map[string]any{"pack_ref": c.packRef, "version": "2026.08.1", "reason": "authority corrected the rate retroactively"}
		for k, v := range body {
			base[k] = v
		}
		return e.do("POST", "/v1/admin/remediation-cases", "tax-lead", base)
	}
	code, r := open(nil)
	mustStatus(t, 201, code, r)
	id := r["case_id"].(string)
	assert.Equal(t, "MANUAL", r["trigger"])
	assert.EqualValues(t, 2, r["decisions_total"])
	assert.Equal(t, 1, e.pub.count(events.EventRemediationOpened))

	// A second call for the same version and scope attaches to the open case instead of opening another.
	code, r = open(nil)
	mustStatus(t, 200, code, r)
	assert.Equal(t, id, r["case_id"])
	assert.Equal(t, 1, e.pub.count(events.EventRemediationOpened), "not announced again")

	// A scope with no decisions, a version never used, and an unknown version are refused.
	code, r = open(map[string]any{"ring": "no-such-ring", "region": "no-such-region"})
	mustStatus(t, 409, code, r)
	assert.Equal(t, "nothing_to_remediate", r["error"])
	code, r = open(map[string]any{"version": "2026.09.1"})
	mustStatus(t, 409, code, r)
	code, _ = open(map[string]any{"version": "9.9.9"})
	assert.Equal(t, 404, code)
	code, _ = open(map[string]any{"reason": ""})
	assert.Equal(t, 400, code)
	code, _ = open(map[string]any{"pack_ref": ""})
	assert.Equal(t, 400, code)
}

func TestRemediation_DatabaseRefusesWhatTheServiceWouldRefuse(t *testing.T) {
	c := releasedPack(t, 0)
	e := c.e
	ids := decideN(t, c, 2)
	code, r := e.do("POST", "/v1/admin/remediation-cases", "tax-lead", map[string]any{"pack_ref": c.packRef, "version": "2026.08.1", "reason": "incident"})
	mustStatus(t, 201, code, r)
	id := r["case_id"].(string)

	// A bypassing writer is stopped too: no close with decisions pending, no self-close, no edits, no deletes.
	_, err := pool.Exec(ctx, `UPDATE remediation_cases SET status='CLOSED', closed_by='someone-else', closed_at=NOW(), closure_note='x' WHERE case_id::text=$1`, id)
	require.Error(t, err, "closing with decisions pending is refused in SQL")
	assert.Contains(t, err.Error(), "no disposition")
	_, err = pool.Exec(ctx, `INSERT INTO remediation_dispositions (case_id, decision_id, disposition, evidence_ref, recorded_by) VALUES ($1::uuid,$2::uuid,'RECONFIRMED','E','a'), ($1::uuid,$3::uuid,'RECONFIRMED','E','a')`, id, ids[0], ids[1])
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE remediation_cases SET status='CLOSED', closed_by='tax-lead', closed_at=NOW(), closure_note='x' WHERE case_id::text=$1`, id)
	require.Error(t, err, "the opener cannot close the case, even in SQL")
	assert.Contains(t, err.Error(), "other than the person who opened")
	_, err = pool.Exec(ctx, `UPDATE remediation_cases SET reason='rewritten' WHERE case_id::text=$1`, id)
	require.Error(t, err, "a case's content cannot be edited")
	_, err = pool.Exec(ctx, `UPDATE remediation_dispositions SET disposition='NOT_AFFECTED' WHERE case_id::text=$1`, id)
	require.Error(t, err, "dispositions are append-only")
	_, err = pool.Exec(ctx, `DELETE FROM remediation_dispositions WHERE case_id::text=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM remediation_case_decisions WHERE case_id::text=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM remediation_cases WHERE case_id::text=$1`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO remediation_dispositions (case_id, decision_id, disposition, evidence_ref, recorded_by) VALUES ($1::uuid,$2::uuid,'RECONFIRMED','','a')`, id, ids[0])
	require.Error(t, err, "a disposition without evidence is refused")
	_, err = pool.Exec(ctx, fmt.Sprintf(`UPDATE rule_decision_evidence SET outcome='X' WHERE decision_id='%s'`, ids[0]))
	require.Error(t, err, "the decision itself stays append-only")

	// A legitimate close by someone else works (the closed-case refusal of further dispositions is proved through the API above).
	_, err = pool.Exec(ctx, `UPDATE remediation_cases SET status='CLOSED', closed_by='reviewer', closed_at=NOW(), closure_note='ok' WHERE case_id::text=$1`, id)
	require.NoError(t, err)
	assert.Equal(t, "CLOSED", versionCaseStatus(t, id))
}
