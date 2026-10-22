package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// A12/B34: a certification can be revoked, prospectively, by someone other
// than its certifier, with a reason and evidence; the sealed certification and
// its history are retained untouched.

func revokeRequest() domain.RevokeCertificationRequest {
	return domain.RevokeCertificationRequest{Reason: "bank statement restated after certification",
		EvidenceRefs: []string{"bank-restatement-2026-10", "ticket:RECON-42"}}
}

func (f *fixture) certifiedRun(name string) *domain.ReconciliationRun {
	f.t.Helper()
	def := f.defineReconciliation(orgA, name, 0)
	run := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(run.RunID)
	return run
}

// certificationRow is the certifications row exactly as stored.
func (f *fixture) certificationRow(runID string) string {
	f.t.Helper()
	var row string
	if err := f.admin.QueryRow(f.ctx, `SELECT row_to_json(c)::text FROM certifications c WHERE run_id = $1`, runID).Scan(&row); err != nil {
		f.t.Fatalf("read certification row: %v", err)
	}
	return row
}

func TestRevocation_RevokesProspectivelyAndRetainsTheCertification(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("revoke-retain")
	before, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rowBefore := f.certificationRow(run.RunID)
	historyBefore := f.transitions(run.RunID)
	certifiedRun, _ := f.s.GetRun(f.ctx, orgA, run.RunID)

	claim := f.claim("RevokeCertification", run.RunID)
	v, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-erin", grant("controller-erin"), claim)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// The record names exactly the certification it revokes, and why, on what
	// evidence and by what authority.
	if !strings.HasPrefix(v.RevocationID, domain.PrefixRevocation) || v.RunID != run.RunID ||
		v.CertificationID != before.CertificationID || v.RevokedManifestSHA256 != before.ManifestSHA256 ||
		v.CertifiedBy != "certifier" || !v.CertifiedAt.Equal(before.SealedAt) || v.CertificationTransitionID == 0 ||
		v.Reason != revokeRequest().Reason || strings.Join(v.EvidenceRefs, ",") != "bank-restatement-2026-10,ticket:RECON-42" ||
		v.RevokedBy != "controller-erin" || v.Authority.AccessDecisionID != "adl-controller-erin" || v.RevokedAt.Before(v.CertifiedAt) {
		t.Fatalf("revocation: %+v", v)
	}

	// Only the certification dimension moved; the run is still Certified, by
	// the same certifier, at the same time, with the same result.
	got, err := f.s.GetRun(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	wantStates(t, got, domain.RunCertified, domain.ResultMatch, domain.CertRevoked)
	if got.CertifiedBy != "certifier" || got.CertifiedAt == nil || !got.CertifiedAt.Equal(*certifiedRun.CertifiedAt) {
		t.Fatalf("certification facts changed: %+v", got)
	}
	// History: everything before is unchanged, and one CERTIFICATION move is appended.
	wantHistory(t, f.transitions(run.RunID), append(historyBefore, "CERTIFICATION:CERTIFIED>REVOKED")...)

	// The sealed certification is untouched — the stored row byte for byte —
	// and is served with the revocation beside it.
	if row := f.certificationRow(run.RunID); row != rowBefore {
		t.Fatalf("certification row changed:\n before %s\n after  %s", rowBefore, row)
	}
	after, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revocation == nil || after.Revocation.RevocationID != v.RevocationID {
		t.Fatalf("certification read without its revocation: %+v", after.Revocation)
	}
	served := *after
	served.Revocation = nil
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(served)
	if string(b1) != string(b2) {
		t.Fatalf("served certification changed:\n before %s\n after  %s", b1, b2)
	}

	// The event carries the prior certification, reason, evidence and effective time.
	var payload []byte
	if err := f.admin.QueryRow(f.ctx, `SELECT payload FROM outbox_events WHERE event_type = 'CONTROL.CertificationRevoked'
		AND aggregate_id = $1`, before.CertificationID).Scan(&payload); err != nil {
		t.Fatalf("revocation event: %v", err)
	}
	var ev domain.CertificationRevocation
	if err := json.Unmarshal(payload, &ev); err != nil || ev.CertificationID != before.CertificationID || ev.Reason == "" ||
		len(ev.EvidenceRefs) != 2 || !ev.RevokedAt.Equal(v.RevokedAt) {
		t.Fatalf("revocation event payload: %s (%v)", payload, err)
	}

	// A retry replays; the same key for another request is refused; a second
	// revocation is refused. None writes anything.
	var replay *domain.IdempotentReplayError
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-erin", grant("controller-erin"), claim); !errors.As(err, &replay) ||
		replay.ResourceID != run.RunID {
		t.Fatalf("retry: %v", err)
	}
	reused := claim
	reused.RequestSHA256 = "different"
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-erin", grant("controller-erin"), reused); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("reused key: %v", err)
	}
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-frank", grant("controller-frank"),
		f.claim("RevokeCertification", run.RunID)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("second revocation: %v", err)
	}
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM reconciliation_certification_revocations WHERE run_id = $1`, run.RunID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("revocations recorded: %d, %v", n, err)
	}

	// Another tenant sees neither the run nor its revocation.
	if _, err := f.s.RevokeCertification(f.ctx, orgB, run.RunID, revokeRequest(), "controller-erin", grant("controller-erin"),
		f.claim("RevokeCertification", "orgB")); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("cross-tenant revoke: %v", err)
	}
	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, setErr := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgB)
	countErr := tx.QueryRow(f.ctx, `SELECT count(*) FROM reconciliation_certification_revocations`).Scan(&n)
	_ = tx.Rollback(f.ctx)
	if setErr != nil || countErr != nil || n != 0 {
		t.Fatalf("cross-tenant read of revocations: %d, %v, %v", n, setErr, countErr)
	}
}

// Independence: whoever is recorded as certifying the run — the sealer, or any
// CERTIFICATION -> CERTIFIED transition's actor, not only the last — cannot
// revoke it, and a refused revocation consumes nothing.
func TestRevocation_CertifierCannotRevoke(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("revoke-independence")
	// An earlier recorded certifier of the same run (a transition row is
	// append-only, so this is how a backfilled certification would appear).
	if _, err := f.admin.Exec(f.ctx, `INSERT INTO reconciliation_run_transitions (tenant_id, run_id, dimension, from_state, to_state, reason, actor)
		VALUES ($1, $2, 'CERTIFICATION', 'ELIGIBLE', 'CERTIFIED', 'earlier certification', 'dave')`, orgA, run.RunID); err != nil {
		t.Fatal(err)
	}

	for _, certifier := range []string{"certifier", "dave"} {
		claim := f.claim("RevokeCertification", run.RunID)
		claim.PrincipalID = certifier
		if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), certifier, grant(certifier), claim); !errors.Is(err, domain.ErrRevokerIsCertifier) {
			t.Fatalf("%s revokes: %v", certifier, err)
		}
		var keys int
		if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM idempotency_keys WHERE principal_id = $1 AND idempotency_key = $2`,
			certifier, claim.Key).Scan(&keys); err != nil || keys != 0 {
			t.Fatalf("a refused revocation consumed its key: %d, %v", keys, err)
		}
	}
	got, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, got, domain.RunCertified, domain.ResultMatch, domain.CertCertified)
}

// Only a Certified run's certification can be revoked, only with a traceable
// authority of the revoker's own, and only with a reason and evidence.
func TestRevocation_RefusesWhatItCannotRevoke(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "revoke-refusals", 0)
	open := f.startRun(orgA, def, cleanPops())
	f.match(open.RunID)
	if _, err := f.s.RevokeCertification(f.ctx, orgA, open.RunID, revokeRequest(), "controller-erin", grant("controller-erin"),
		f.claim("RevokeCertification", open.RunID)); !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("revoke an uncertified run: %v", err)
	}

	run := f.certifiedRun("revoke-refusals-certified")
	denied := grant("controller-erin")
	denied.DecisionOutcome = "DENIED"
	for name, a := range map[string]*domain.AuthoritySnapshot{
		"no authority": nil, "denied": denied, "another principal's": grant("someone-else"),
	} {
		if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-erin", a,
			f.claim("RevokeCertification", run.RunID)); !errors.Is(err, domain.ErrRevokerAuthorityUntraceable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, req := range map[string]domain.RevokeCertificationRequest{
		"no reason":   {EvidenceRefs: []string{"x"}},
		"no evidence": {Reason: "restated"},
	} {
		if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, req, "controller-erin", grant("controller-erin"),
			f.claim("RevokeCertification", run.RunID)); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, got, domain.RunCertified, domain.ResultMatch, domain.CertCertified)
}

// A revoked run can still be superseded; it keeps REVOKED, and its
// certification and revocation are both still served.
func TestRevocation_RevokedRunCanBeSuperseded(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "revoke-supersede", 0)
	old := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(old.RunID)
	if _, err := f.s.RevokeCertification(f.ctx, orgA, old.RunID, revokeRequest(), "controller-erin", grant("controller-erin"),
		f.claim("RevokeCertification", old.RunID)); err != nil {
		t.Fatal(err)
	}
	replacement := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(replacement.RunID)
	superseded, err := f.s.SupersedeRun(f.ctx, orgA, old.RunID, domain.SupersedeRunRequest{NewRunID: replacement.RunID,
		Reason: "re-run on the restated statement"}, "controller-carol", f.claim("Supersede", old.RunID))
	if err != nil {
		t.Fatalf("supersede a revoked run: %v", err)
	}
	wantStates(t, superseded, domain.RunSuperseded, domain.ResultSuperseded, domain.CertRevoked)
	cert, err := f.s.GetCertification(f.ctx, orgA, old.RunID)
	if err != nil || cert.Revocation == nil || cert.Revocation.RevokedBy != "controller-erin" {
		t.Fatalf("superseded revoked certification: %+v, %v", cert, err)
	}
}

// The database refuses every way around the command: REVOKED with no record,
// a record with no revocation, a revocation that changes anything else, a
// record breaking its CHECKs, and rewriting or deleting a record.
func TestRevocation_DatabaseRefusesWritesAround(t *testing.T) {
	f := newFixture(t)
	revoked := f.certifiedRun("revoke-db-revoked")
	if _, err := f.s.RevokeCertification(f.ctx, orgA, revoked.RunID, revokeRequest(), "controller-erin", grant("controller-erin"),
		f.claim("RevokeCertification", revoked.RunID)); err != nil {
		t.Fatal(err)
	}
	run := f.certifiedRun("revoke-db")

	// A revocation row for run, built from its own certification.
	insert := func(reason, refs, authority, revokedBy string) string {
		return `INSERT INTO reconciliation_certification_revocations (revocation_id, tenant_id, run_id, certification_id,
				revoked_manifest_sha256, certification_transition_id, certified_by, certified_at, reason, evidence_refs,
				authority_snapshot, revoked_by)
			SELECT 'rcv_` + uuid.NewString() + `', c.tenant_id, c.run_id, c.certification_id, c.manifest_sha256,
				(SELECT max(transition_id) FROM reconciliation_run_transitions t WHERE t.run_id = c.run_id
				 AND t.dimension = 'CERTIFICATION' AND t.to_state = 'CERTIFIED'),
				c.sealed_by, c.sealed_at, ` + reason + `, ` + refs + `, ` + authority + `, ` + revokedBy + `
			FROM certifications c WHERE c.run_id = '` + run.RunID + `'`
	}
	const okAuthority = `'{"principal_id":"controller-erin","access_decision_id":"adl-1","decision_outcome":"GRANTED"}'::jsonb`

	for name, tc := range map[string]struct {
		stmts []string
		want  string
	}{
		"REVOKED without a record": {[]string{`UPDATE reconciliation_runs SET certification_state = 'REVOKED' WHERE run_id = '` + run.RunID + `'`},
			"without its revocation record"},
		"a record without a revocation": {[]string{insert(`'restated'`, `ARRAY['x']`, okAuthority, `'controller-erin'`)},
			"whose certification is not REVOKED"},
		"a revocation that changes another column": {[]string{insert(`'restated'`, `ARRAY['x']`, okAuthority, `'controller-erin'`),
			`UPDATE reconciliation_runs SET certification_state = 'REVOKED', certified_by = 'controller-erin' WHERE run_id = '` + run.RunID + `'`},
			"nothing else"},
		"revoked by the certifier": {[]string{insert(`'restated'`, `ARRAY['x']`,
			`'{"principal_id":"certifier","access_decision_id":"adl-1","decision_outcome":"GRANTED"}'::jsonb`, `'certifier'`)},
			"reconciliation_certification_revocations_independence_check"},
		"blank reason": {[]string{insert(`'  '`, `ARRAY['x']`, okAuthority, `'controller-erin'`)},
			"reconciliation_certification_revocations_reason_check"},
		"no evidence": {[]string{insert(`'restated'`, `ARRAY[]::text[]`, okAuthority, `'controller-erin'`)},
			"reconciliation_certification_revocations_evidence_refs_check"},
		"blank evidence": {[]string{insert(`'restated'`, `ARRAY['x', ' ']`, okAuthority, `'controller-erin'`)},
			"reconciliation_certification_revocations_evidence_refs_check"},
		"another principal's authority": {[]string{insert(`'restated'`, `ARRAY['x']`,
			`'{"principal_id":"someone-else","access_decision_id":"adl-1","decision_outcome":"GRANTED"}'::jsonb`, `'controller-erin'`)},
			"reconciliation_certification_revocations_authority_check"},
		"authority without a decision": {[]string{insert(`'restated'`, `ARRAY['x']`,
			`'{"principal_id":"controller-erin","decision_outcome":"GRANTED"}'::jsonb`, `'controller-erin'`)},
			"reconciliation_certification_revocations_authority_check"},
		"record rewritten": {[]string{`UPDATE reconciliation_certification_revocations SET reason = 'x' WHERE run_id = '` + revoked.RunID + `'`},
			"immutable"},
		"record deleted": {[]string{`DELETE FROM reconciliation_certification_revocations WHERE run_id = '` + revoked.RunID + `'`},
			"immutable"},
		"revoked run re-certified": {[]string{`UPDATE reconciliation_runs SET certification_state = 'CERTIFIED' WHERE run_id = '` + revoked.RunID + `'`},
			"invalid reconciliation run certification transition"},
	} {
		tx, err := f.app.Begin(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, setErr := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA)
		for _, stmt := range tc.stmts {
			if _, err = tx.Exec(f.ctx, stmt); err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit(f.ctx) // the deferred record check fires here
		}
		_ = tx.Rollback(f.ctx) // before any Fatal: a leaked transaction blocks the pool's Close
		if setErr != nil {
			t.Fatal(setErr)
		}
		var pgErr *pgconn.PgError
		if err == nil || !errors.As(err, &pgErr) || !strings.Contains(pgErr.Message+" "+pgErr.ConstraintName, tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}
	got, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, got, domain.RunCertified, domain.ResultMatch, domain.CertCertified)
}

// 000011's down refuses while any revocation is recorded, and round-trips
// while none is.
func TestMigration000011_DownRefusesWhileRevocationsExist(t *testing.T) {
	f := newFixture(t)
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	down, err := os.ReadFile(filepath.Join(dir, "000011_certification_revocation.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(dir, "000011_certification_revocation.up.sql"))
	if err != nil {
		t.Fatal(err)
	}

	// Nothing revoked yet (a certified run exists): down and up again.
	run := f.certifiedRun("revoke-migration")
	if _, err := f.admin.Exec(f.ctx, string(down)); err != nil {
		t.Fatalf("down with no revocation: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	// The table is new again, so the app role's grant must be too.
	if _, err := f.admin.Exec(f.ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON reconciliation_certification_revocations TO zoiko_app_test`); err != nil {
		t.Fatal(err)
	}

	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, revokeRequest(), "controller-erin", grant("controller-erin"),
		f.claim("RevokeCertification", run.RunID)); err != nil {
		t.Fatalf("revoke after the round trip: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(down)); err == nil || !strings.Contains(err.Error(), "cannot roll back 000011") {
		t.Fatalf("down with a revocation: %v", err)
	}
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM reconciliation_certification_revocations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("refused down changed the revocations: %d, %v", n, err)
	}
	got, _ := f.s.GetRun(f.ctx, orgA, run.RunID)
	wantStates(t, got, domain.RunCertified, domain.ResultMatch, domain.CertRevoked)
}
