//go:build integration

// ZS-JUR-001 Wave 8 end to end: a jurisdiction is supported for production only
// after its rollout meets the Definition of Ready and Definition of Done with
// independent attestations, a qualified expert approves it, and a linked pack has a
// RELEASED version. Governance only: no regulatory content is created.
package registryit_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func attestAll(t *testing.T, s *payrollScenario, id, checklist, prefix string, n int, who string) {
	t.Helper()
	for i := 1; i <= n; i++ {
		code, r := s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", who, map[string]any{
			"checklist": checklist, "item_code": fmt.Sprintf("%s_%02d", prefix, i), "met": true, "evidence_ref": fmt.Sprintf("doc://evidence/%s/%02d", prefix, i)})
		mustStatus(t, 201, code, r)
	}
}

func transition(t *testing.T, s *payrollScenario, id, who, to, reason string, want int) map[string]any {
	t.Helper()
	body := map[string]any{"to": to}
	if reason != "" {
		body["reason"] = reason
	}
	code, r := s.e.do("POST", "/v1/admin/rollouts/"+id+"/transition", who, body)
	require.Equal(t, want, code, "%v", r)
	return r
}

func blockersOf(r map[string]any) string {
	var out []string
	if b, ok := r["blockers"].([]any); ok {
		for _, x := range b {
			out = append(out, x.(string))
		}
	}
	return strings.Join(out, " | ")
}

func supportOf(t *testing.T, s *payrollScenario, code string) map[string]any {
	t.Helper()
	c, r := s.e.do("GET", "/v1/jurisdiction-support/"+code, "client", nil)
	mustStatus(t, 200, c, r)
	return r
}

func TestRollout_ASeededPortfolioIsIntentOnlyAndSupportIsExplicit(t *testing.T) {
	s := newRecordsScenario(t)
	code, r := s.e.do("POST", "/v1/admin/rollouts:seed-portfolio", "platform", nil)
	mustStatus(t, 200, code, r)
	assert.EqualValues(t, 10, r["created"].(float64)+r["already_present"].(float64), "the s32 initial portfolio has ten families")
	code, r = s.e.do("POST", "/v1/admin/rollouts:seed-portfolio", "platform", nil)
	mustStatus(t, 200, code, r)
	assert.EqualValues(t, 0, r["created"], "seeding is idempotent")
	assert.EqualValues(t, 10, r["already_present"])

	// A seeded jurisdiction is NOT supported: the portfolio states intent only (s32).
	sup := supportOf(t, s, "GB")
	assert.Equal(t, false, sup["supported"])
	assert.Equal(t, "NOT_YET_SUPPORTED", sup["outcome"])
	sup = supportOf(t, s, "ATLANTIS")
	assert.Equal(t, "NO_ROLLOUT", sup["outcome"], "JUR-NEG-22: an unknown jurisdiction is explicitly unsupported")

	// A seeded entry has no owner, so authoring cannot start.
	code, list := s.e.do("GET", "/v1/admin/rollouts?status=PLANNED", "platform", nil)
	mustStatus(t, 200, code, list)
	var gbID string
	for _, x := range list["rollouts"].([]any) {
		if m := x.(map[string]any); m["family_ref"] == "gb" {
			gbID = m["rollout_id"].(string)
		}
	}
	require.NotEmpty(t, gbID)
	res := transition(t, s, gbID, "platform", "AUTHORING", "", 409)
	assert.Contains(t, blockersOf(res), "owner")
	code, r = s.e.do("PUT", "/v1/admin/rollouts/"+gbID+"/owner", "platform", map[string]any{"owner": "UNASSIGNED"})
	assert.Equal(t, 400, code, "%v", r)
	code, r = s.e.do("PUT", "/v1/admin/rollouts/"+gbID+"/owner", "platform", map[string]any{"owner": "uk-tax-lead", "support_owner": "uk-support"})
	mustStatus(t, 200, code, r)
	transition(t, s, gbID, "uk-tax-lead", "AUTHORING", "", 200)

	code, c := s.e.do("GET", "/v1/rollout-checklists", "platform", nil)
	mustStatus(t, 200, code, c)
	assert.Len(t, c["definition_of_ready"], 10)
	assert.Len(t, c["definition_of_done"], 11)
}

func TestRollout_LaunchGateAndIndependence(t *testing.T) {
	s := newRecordsScenario(t)
	s.release(t)
	jur := s.jur.JurisdictionCode

	code, r := s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "zz." + strings.ToLower(s.packRef[len(s.packRef)-6:]), "display_name": "Test land", "layer": "COUNTRY",
		"jurisdiction_code": jur, "owner": "owner-1", "support_owner": "support-1", "scope_note": "integration test"})
	mustStatus(t, 201, code, r)
	id := r["rollout_id"].(string)
	code, _ = s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "BAD REF", "display_name": "x", "layer": "COUNTRY", "owner": "o", "jurisdiction_code": "X"})
	assert.Equal(t, 400, code)
	code, _ = s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "no.code", "display_name": "x", "layer": "COUNTRY", "owner": "o"})
	assert.Equal(t, 400, code, "a country rollout must name its jurisdiction")
	code, _ = s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "zz.dup", "display_name": "x", "layer": "GLOBAL_CORE", "owner": "o"})
	assert.Equal(t, 201, code)
	code, _ = s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "zz.dup", "display_name": "x", "layer": "GLOBAL_CORE", "owner": "o"})
	assert.Equal(t, 409, code, "one rollout per family")

	sup := supportOf(t, s, jur)
	assert.Equal(t, "NOT_YET_SUPPORTED", sup["outcome"], "a PLANNED rollout is not support, even though a released pack exists")

	transition(t, s, id, "owner-1", "AUTHORING", "", 200)
	// Cannot skip to LAUNCHED.
	res := transition(t, s, id, "launcher-1", "LAUNCHED", "", 409)
	assert.Contains(t, blockersOf(res), "cannot move to LAUNCHED")

	// Definition of Ready: independent attestations with evidence.
	code, r = s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", "owner-1", map[string]any{"checklist": "READY", "item_code": "DOR_01", "met": true, "evidence_ref": "doc://x"})
	assert.Equal(t, 403, code, "the owner cannot attest to their own rollout: %v", r)
	code, _ = s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", "attester-1", map[string]any{"checklist": "READY", "item_code": "DOR_01", "met": true})
	assert.Equal(t, 400, code, "a met item needs evidence")
	code, _ = s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", "attester-1", map[string]any{"checklist": "READY", "item_code": "DOR_99", "met": true, "evidence_ref": "x"})
	assert.Equal(t, 400, code)
	code, _ = s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", "attester-1", map[string]any{"checklist": "DONE", "item_code": "DOR_01", "met": true, "evidence_ref": "x"})
	assert.Equal(t, 400, code, "an item belongs to its own checklist")
	attestAll(t, s, id, "READY", "DOR", 9, "attester-1")
	res = transition(t, s, id, "owner-1", "READY", "", 409)
	assert.Contains(t, blockersOf(res), "DOR_10")
	attestAll(t, s, id, "READY", "DOR", 10, "attester-1")
	transition(t, s, id, "owner-1", "READY", "", 200)

	// Launch gate: everything is reported at once.
	res = transition(t, s, id, "launcher-1", "LAUNCHED", "", 409)
	b := blockersOf(res)
	for _, want := range []string{"Definition of Done is incomplete", "DOD_01", "expert", "no pack is linked"} {
		assert.Contains(t, b, want)
	}
	attestAll(t, s, id, "DONE", "DOD", 11, "attester-2")

	// A qualified expert, independent of the owner.
	code, r = s.e.do("POST", "/v1/admin/rollouts/"+id+"/expert-approvals", "owner-1", map[string]any{"qualification": "Chartered tax adviser", "scope": "VAT", "decision": "APPROVE"})
	assert.Equal(t, 403, code, "%v", r)
	code, _ = s.e.do("POST", "/v1/admin/rollouts/"+id+"/expert-approvals", "expert-1", map[string]any{"qualification": " ", "scope": "VAT", "decision": "APPROVE"})
	assert.Equal(t, 400, code, "a qualification is required")
	code, r = s.e.do("POST", "/v1/admin/rollouts/"+id+"/expert-approvals", "expert-1", map[string]any{"qualification": "Chartered tax adviser", "scope": "VAT and payroll", "decision": "APPROVE"})
	mustStatus(t, 201, code, r)

	res = transition(t, s, id, "launcher-1", "LAUNCHED", "", 409)
	assert.Contains(t, blockersOf(res), "no pack is linked")
	code, r = s.e.do("PUT", "/v1/admin/rollouts/"+id+"/packs", "platform", map[string]any{"pack_refs": []string{"nope.missing"}})
	assert.Equal(t, 404, code, "%v", r)
	code, r = s.e.do("PUT", "/v1/admin/rollouts/"+id+"/packs", "platform", map[string]any{"pack_refs": []string{s.packRef}})
	mustStatus(t, 200, code, r)

	// Independence of the launcher.
	assert.Contains(t, blockersOf(transition(t, s, id, "owner-1", "LAUNCHED", "", 409)), "owner cannot launch")
	assert.Contains(t, blockersOf(transition(t, s, id, "expert-1", "LAUNCHED", "", 409)), "expert cannot launch")
	transition(t, s, id, "launcher-1", "LAUNCHED", "", 200)

	sup = supportOf(t, s, jur)
	assert.Equal(t, true, sup["supported"])
	assert.Equal(t, "SUPPORTED", sup["outcome"])

	// The record of how it got there.
	code, d := s.e.do("GET", "/v1/admin/rollouts/"+id, "auditor", nil)
	mustStatus(t, 200, code, d)
	assert.Len(t, d["events"], 3)
	assert.Equal(t, []any{s.packRef}, d["pack_refs"])
	assert.Len(t, d["checklist"], 21)

	// Suspend needs a reason; the latest attestation counts, so a retracted item blocks relaunch.
	transition(t, s, id, "ops", "SUSPENDED", "", 409)
	transition(t, s, id, "ops", "SUSPENDED", "authority changed the schema", 200)
	assert.Equal(t, "SUSPENDED", supportOf(t, s, jur)["outcome"])
	code, r = s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", "attester-2", map[string]any{"checklist": "DONE", "item_code": "DOD_05", "met": false})
	mustStatus(t, 201, code, r)
	assert.Contains(t, blockersOf(transition(t, s, id, "launcher-1", "LAUNCHED", "", 409)), "DOD_05")
	code, r = s.e.do("POST", "/v1/admin/rollouts/"+id+"/attestations", "attester-2", map[string]any{"checklist": "DONE", "item_code": "DOD_05", "met": true, "evidence_ref": "doc://reverified"})
	mustStatus(t, 201, code, r)
	transition(t, s, id, "launcher-1", "LAUNCHED", "", 200)
	assert.Equal(t, "SUPPORTED", supportOf(t, s, jur)["outcome"])

	// A withdrawn pack ends support even though the rollout stays LAUNCHED (JUR-NEG-28).
	_, err := pool.Exec(ctx, `ALTER TABLE jurisdiction_pack_versions DISABLE TRIGGER USER`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_pack_versions SET status='WITHDRAWN' WHERE pack_id=(SELECT pack_id FROM jurisdiction_packs WHERE pack_ref=$1)`, s.packRef)
	_, _ = pool.Exec(ctx, `ALTER TABLE jurisdiction_pack_versions ENABLE TRIGGER USER`)
	require.NoError(t, err)
	sup = supportOf(t, s, jur)
	assert.Equal(t, false, sup["supported"])
	assert.Equal(t, "NO_RELEASED_PACK", sup["outcome"])

	assert.Equal(t, 404, func() int { c, _ := s.e.do("GET", "/v1/admin/rollouts/not-a-uuid", "auditor", nil); return c }())
	transition(t, s, id, "ops", "RETIRED", "superseded", 200)
	transition(t, s, id, "ops", "LAUNCHED", "", 409)
}

func TestRollout_DatabaseRefusesWhatTheServiceWouldRefuse(t *testing.T) {
	s := newRecordsScenario(t)
	code, r := s.e.do("POST", "/v1/admin/rollouts", "platform", map[string]any{"family_ref": "zz.db." + strings.ToLower(s.regimeID[:6]), "display_name": "DB attack", "layer": "GLOBAL_CORE", "owner": "owner-9"})
	mustStatus(t, 201, code, r)
	id := r["rollout_id"].(string)

	_, err := pool.Exec(ctx, `UPDATE jurisdiction_rollouts SET status='LAUNCHED' WHERE rollout_id=$1::uuid`, id)
	require.Error(t, err, "PLANNED cannot jump to LAUNCHED")
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_rollouts SET status='AUTHORING' WHERE rollout_id=$1::uuid`, id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE jurisdiction_rollouts SET status='READY' WHERE rollout_id=$1::uuid`, id)
	require.Error(t, err, "the Definition of Ready gate holds in SQL too")
	assert.Contains(t, err.Error(), "Definition of Ready")

	_, err = pool.Exec(ctx, `INSERT INTO rollout_attestations (rollout_id, checklist, item_code, met, evidence_ref, attested_by) VALUES ($1::uuid,'READY','DOR_01',true,'x','owner-9')`, id)
	require.Error(t, err, "the owner cannot attest in SQL either")
	_, err = pool.Exec(ctx, `INSERT INTO rollout_attestations (rollout_id, checklist, item_code, met, evidence_ref, attested_by) VALUES ($1::uuid,'READY','DOR_01',true,'  ','someone')`, id)
	require.Error(t, err, "a met item needs evidence")
	_, err = pool.Exec(ctx, `INSERT INTO rollout_attestations (rollout_id, checklist, item_code, met, evidence_ref, attested_by) VALUES ($1::uuid,'READY','DOD_01',true,'x','someone')`, id)
	require.Error(t, err, "an item code must belong to its checklist")
	_, err = pool.Exec(ctx, `INSERT INTO rollout_attestations (rollout_id, checklist, item_code, met, evidence_ref, attested_by) VALUES ($1::uuid,'READY','DOR_01',true,'doc://ok','someone')`, id)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE rollout_attestations SET met=false WHERE rollout_id=$1::uuid`, id)
	require.Error(t, err, "attestations are append-only")
	_, err = pool.Exec(ctx, `DELETE FROM rollout_attestations WHERE rollout_id=$1::uuid`, id)
	require.Error(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM jurisdiction_rollouts WHERE rollout_id=$1::uuid`, id)
	require.Error(t, err, "rollouts are never deleted")
}
