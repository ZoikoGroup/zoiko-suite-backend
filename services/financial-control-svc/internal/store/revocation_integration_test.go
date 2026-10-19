//go:build integration

package store_test

// SPEC_DEVIATIONS A1/B6 against a real Postgres: revoking a certification.

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/store"
)

var revokeReq = domain.RevokeCertificationRequest{
	Reason:       "source totals restated after certification",
	EvidenceRefs: []string{"doc:restatement-123", "ticket:FIN-88"},
}

// certifiedRun runs FIN-CTRL-020 to a PASS (created and executed by "prep")
// and has "checker" certify it.
func certifiedRun(t *testing.T, w *wave2) *domain.ControlRun {
	t.Helper()
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, final := w.run("FIN-CTRL-020", fiscal)
	require.Equal(t, domain.LifecycleReadyToCertify, final.LifecycleState)
	out, err := testStore.CertifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version,
		domain.CertifyRequest{Decision: "CERTIFY", Reason: "reviewed"})
	require.NoError(t, err)
	require.Equal(t, domain.CertCertified, out.CertificationState)
	return out
}

// transitionsJSON is every transition row of a run, as Postgres renders it.
func transitionsJSON(t *testing.T, runID string) []string {
	t.Helper()
	rows, err := testPool.Query(ctx, `SELECT row_to_json(t)::text FROM control_run_transitions t
		WHERE run_id = $1 ORDER BY transition_id`, runID)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestRevocation_RevokesCertificationAndRetainsTheOriginal(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	before := transitionsJSON(t, run.RunID)
	var certRow string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT row_to_json(t)::text FROM control_run_transitions t
		WHERE run_id = $1 AND dimension = 'CERTIFICATION' AND to_state = 'CERTIFIED'`, run.RunID).Scan(&certRow))

	out, replay, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "corr-revoke", "revoke-"+uuid.NewString(), run.Version, revokeReq)
	require.NoError(t, err)
	assert.False(t, replay)
	assert.Equal(t, domain.LifecycleCertified, out.LifecycleState, "the lifecycle does not move")
	assert.Equal(t, domain.CertRevoked, out.CertificationState)
	assert.Equal(t, run.ResultState, out.ResultState, "the result is untouched")
	assert.Equal(t, run.Version+1, out.Version)

	// Additive only: every pre-existing transition row, the certification
	// included, is byte-identical, and exactly one row was appended.
	after := transitionsJSON(t, run.RunID)
	require.Len(t, after, len(before)+1)
	assert.Equal(t, before, after[:len(before)], "prior history must not change")
	assert.Contains(t, after[:len(before)], certRow, "the original certification is retained as it was")
	var last struct {
		Dimension, FromState, ToState, Reason, ActorID string
	}
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(after[len(after)-1]), &m))
	last.Dimension, last.FromState, last.ToState = m["dimension"].(string), m["from_state"].(string), m["to_state"].(string)
	last.Reason, last.ActorID = m["reason"].(string), m["actor_id"].(string)
	assert.Equal(t, "CERTIFICATION", last.Dimension)
	assert.Equal(t, "CERTIFIED", last.FromState)
	assert.Equal(t, "REVOKED", last.ToState)
	assert.Equal(t, revokeReq.Reason, last.Reason)
	assert.Equal(t, "auditor", last.ActorID)

	// The revocation record names the certification it revokes, and its evidence.
	var certTransitionID int64
	var certifiedBy, revokedBy, reason string
	var evidence []string
	require.NoError(t, testPool.QueryRow(ctx, `SELECT certification_transition_id, certified_by, revoked_by, reason, evidence_refs
		FROM control_certification_revocations WHERE run_id = $1`, run.RunID).
		Scan(&certTransitionID, &certifiedBy, &revokedBy, &reason, &evidence))
	var certTransitionInRow float64
	require.NoError(t, json.Unmarshal([]byte(certRow), &m))
	certTransitionInRow = m["transition_id"].(float64)
	assert.Equal(t, int64(certTransitionInRow), certTransitionID)
	assert.Equal(t, "checker", certifiedBy)
	assert.Equal(t, "auditor", revokedBy)
	assert.Equal(t, revokeReq.Reason, reason)
	assert.Equal(t, revokeReq.EvidenceRefs, evidence)

	// The event carries the prior certification, reason, evidence and effective time.
	var payload map[string]any
	require.NoError(t, testPool.QueryRow(ctx, `SELECT payload FROM outbox_events
		WHERE aggregate_id = $1 AND event_type = 'control.run.certification_revoked'`, run.RunID).Scan(&payload))
	assert.Equal(t, "CERTIFIED", payload["prior_certification_state"])
	assert.Equal(t, "REVOKED", payload["certification_state"])
	assert.Equal(t, "checker", payload["certified_by"])
	assert.Equal(t, revokeReq.Reason, payload["reason"])
	assert.Len(t, payload["evidence_refs"], 2)
	assert.NotEmpty(t, payload["effective_at"])

	// The record itself is append-only.
	_, err = testPool.Exec(ctx, `UPDATE control_certification_revocations SET reason = 'nothing' WHERE run_id = $1`, run.RunID)
	assert.Error(t, err, "a revocation is never rewritten")
	_, err = testPool.Exec(ctx, `DELETE FROM control_certification_revocations WHERE run_id = $1`, run.RunID)
	assert.Error(t, err, "a revocation is never deleted")
}

func TestRevocation_CertifierCannotRevokeTheirOwnCertification(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	_, _, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "checker", "c", "k-"+uuid.NewString(), run.Version, revokeReq)
	require.ErrorIs(t, err, domain.ErrRevokerIsCertifier)

	got, err := testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.CertCertified, got.CertificationState, "a refused revocation changes nothing")
	assert.Equal(t, run.Version, got.Version)

	// The run's preparer is not the certifier, and may revoke.
	_, _, err = testStore.RevokeCertification(ctx, w.tenant, run.RunID, "prep", "c", "k-"+uuid.NewString(), run.Version, revokeReq)
	require.NoError(t, err)
}

func TestRevocation_RevokedIsTerminal_AndTheKeyReplays(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	key := "revoke-" + uuid.NewString()
	first, replay, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "c", key, run.Version, revokeReq)
	require.NoError(t, err)
	require.False(t, replay)

	// A retry with the same key, even carrying the now-stale version, gets the
	// original result rather than an error, and records nothing new.
	again, replay, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "c", key, run.Version, revokeReq)
	require.NoError(t, err)
	assert.True(t, replay)
	assert.Equal(t, first.Version, again.Version)

	// A second revocation is refused: REVOKED has no outgoing edge.
	_, _, err = testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor-2", "c", "other-"+uuid.NewString(), store.SkipVersionCheck, revokeReq)
	require.ErrorIs(t, err, domain.ErrInvalidTransition)

	var n int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM control_certification_revocations WHERE run_id = $1`, run.RunID).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM control_run_transitions
		WHERE run_id = $1 AND to_state = 'REVOKED'`, run.RunID).Scan(&n))
	assert.Equal(t, 1, n)

	// The same key on another run is a conflict, not a replay.
	other := certifiedRun(t, w)
	_, _, err = testStore.RevokeCertification(ctx, w.tenant, other.RunID, "auditor", "c", key, other.Version, revokeReq)
	require.ErrorIs(t, err, domain.ErrConflict)
}

func TestRevocation_OnlyACertifiedRunAndOnlyAtTheVersionSeen(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, ready := w.run("FIN-CTRL-020", fiscal)
	_, _, err := testStore.RevokeCertification(ctx, w.tenant, ready.RunID, "auditor", "c", "k-"+uuid.NewString(), store.SkipVersionCheck, revokeReq)
	require.ErrorIs(t, err, domain.ErrInvalidTransition, "a READY_TO_CERTIFY run has no certification to revoke")

	run := certifiedRun(t, w)
	_, _, err = testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "c", "k-"+uuid.NewString(), run.Version-1, revokeReq)
	require.ErrorIs(t, err, domain.ErrConflict, "a stale ETag is refused")

	_, _, err = testStore.RevokeCertification(ctx, w.tenant, uuid.NewString(), "auditor", "c", "k-"+uuid.NewString(), store.SkipVersionCheck, revokeReq)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

// RB-05 "prospective": the close gate, which financial-close-svc's readiness
// check reads, reports the control as blocking from the revocation on, with
// its own reason, time and cause. Nothing is reopened or withdrawn.
func TestRevocation_CloseGateReflectsIt(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	w.src.set(catalogue.SysGL, "control-account-postings", "wm")
	w.src.set(catalogue.SysGL, "manual-journals", "wm", attrs(r("m1", "m1", "10"), "review_gap", ""))
	var certified020 *domain.ControlRun
	for _, code := range []string{"FIN-CTRL-020", "FIN-CTRL-021", "FIN-CTRL-023"} {
		_, final := w.run(code, fiscal)
		out, err := testStore.CertifyRun(ctx, w.tenant, final.RunID, "checker", "c", final.Version, domain.CertifyRequest{Decision: "CERTIFY"})
		require.NoError(t, err, code)
		if code == "FIN-CTRL-020" {
			certified020 = out
		}
	}
	g, err := testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	require.True(t, g.Open)

	_, _, err = testStore.RevokeCertification(ctx, w.tenant, certified020.RunID, "auditor", "c", "k-"+uuid.NewString(), certified020.Version, revokeReq)
	require.NoError(t, err)

	g, err = testStore.CloseGate(ctx, w.tenant, w.entity, "2026-09")
	require.NoError(t, err)
	assert.False(t, g.Open)
	assert.Equal(t, 1, g.Blocking)
	var item domain.CloseGateItem
	for _, it := range g.Items {
		if it.ControlCode == "FIN-CTRL-020" {
			item = it
		}
	}
	assert.Equal(t, domain.GateBlocking, item.Status)
	assert.Equal(t, "CERTIFICATION_REVOKED", item.Reason)
	assert.Equal(t, certified020.RunID, item.RunID)
	assert.Equal(t, "CERTIFIED", item.Lifecycle)
	assert.Equal(t, "REVOKED", item.Certificatio)
	assert.Equal(t, revokeReq.Reason, item.RevocationReason)
	assert.NotEmpty(t, item.CertificationRevokedAt)
}

// A revoked certification can still be replaced: a later run naming it as
// prior supersedes it. Its lifecycle moves to SUPERSEDED and its
// certification stays REVOKED, so the history says it was revoked, then
// replaced.
func TestRevocation_RevokedRunCanStillBeSuperseded(t *testing.T) {
	w := newWave5(t)
	run := certifiedRun(t, w)
	_, _, err := testStore.RevokeCertification(ctx, w.tenant, run.RunID, "auditor", "c", "k-"+uuid.NewString(), run.Version, revokeReq)
	require.NoError(t, err)

	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, next := w.run("FIN-CTRL-020", func(r *domain.CreateRunRequest) {
		fiscal(r)
		r.PriorRunID, r.Reason = run.RunID, "replaces the revoked certification"
	})
	require.Equal(t, domain.LifecycleReadyToCertify, next.LifecycleState)

	prior, err := testStore.GetRun(ctx, w.tenant, run.RunID)
	require.NoError(t, err)
	assert.Equal(t, domain.LifecycleSuperseded, prior.LifecycleState)
	assert.Equal(t, domain.CertRevoked, prior.CertificationState)
	require.NotNil(t, prior.SupersededByRunID)
	assert.Equal(t, next.RunID, *prior.SupersededByRunID)
}

// The database, not only the service, enforces the combinations.
func TestRevocation_ConstraintsHold(t *testing.T) {
	w := newWave5(t)
	w.src.set(catalogue.SysGL, "journal-balances", "wm", journalBal("j1", "5", "5"))
	_, ready := w.run("FIN-CTRL-020", fiscal)
	_, err := testPool.Exec(ctx, `UPDATE control_runs SET certification_state = 'REVOKED' WHERE run_id = $1`, ready.RunID)
	assert.ErrorContains(t, err, "chk_revoked_follows_certification", "REVOKED only follows a certification")

	run := certifiedRun(t, w)
	_, err = testPool.Exec(ctx, `UPDATE control_runs SET certification_state = 'PENDING' WHERE run_id = $1`, run.RunID)
	assert.ErrorContains(t, err, "chk_certified_lifecycle_certification", "lifecycle CERTIFIED admits only CERTIFIED or REVOKED")

	var certTransitionID int64
	require.NoError(t, testPool.QueryRow(ctx, `SELECT transition_id FROM control_run_transitions
		WHERE run_id = $1 AND to_state = 'CERTIFIED' AND dimension = 'CERTIFICATION'`, run.RunID).Scan(&certTransitionID))
	_, err = testPool.Exec(ctx, `INSERT INTO control_certification_revocations
		(tenant_id, run_id, certification_transition_id, certified_by, certified_at, reason, evidence_refs, revoked_by)
		VALUES ($1, $2, $3, 'checker', now(), 'x', ARRAY['doc:1'], 'checker')`, w.tenant, run.RunID, certTransitionID)
	assert.Error(t, err, "the certifier cannot be the revoker, even by a direct write")
	_, err = testPool.Exec(ctx, `INSERT INTO control_certification_revocations
		(tenant_id, run_id, certification_transition_id, certified_by, certified_at, reason, evidence_refs, revoked_by)
		VALUES ($1, $2, $3, 'checker', now(), 'x', ARRAY[]::text[], 'auditor')`, w.tenant, run.RunID, certTransitionID)
	assert.Error(t, err, "a revocation without evidence is refused (NP-47)")
}
