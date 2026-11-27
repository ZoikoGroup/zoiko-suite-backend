package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "blast radius of a revocation"): what relied on a
// certification is recorded, and revoking the certification names exactly that set.

func rely(service, typ, id string) domain.RecordRelianceRequest {
	return domain.RecordRelianceRequest{RelyingService: service, RelyingObjectType: typ, RelyingObjectID: id, Purpose: "built on the certified invoices"}
}

func (f *fixture) relyOn(runID string, req domain.RecordRelianceRequest) (*domain.CertificationReliance, bool, error) {
	return f.s.RecordReliance(f.ctx, orgA, runID, "downstream-svc", req)
}

func (f *fixture) eventsOf(aggregateID, eventType string) []map[string]any {
	f.t.Helper()
	rows, err := f.admin.Query(f.ctx, `SELECT payload FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2 ORDER BY created_at, outbox_event_id`, aggregateID, eventType)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var p map[string]any
		if err := rows.Scan(&p); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestReliance_RecordedIdempotentlyAndListedAsTheBlastRadius(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("reliance-basic")

	a, created, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1"))
	if err != nil || !created || a.CertificationID != cert.CertificationID || a.RunID != run.RunID || a.RecordedBy != "downstream-svc" {
		t.Fatalf("first reliance: %+v created=%v err=%v", a, created, err)
	}
	// The same reliance again is one fact: the first record comes back, nothing is added, no second event.
	again, created, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1"))
	if err != nil || created || again.RelianceID != a.RelianceID {
		t.Fatalf("repeat: %+v created=%v err=%v", again, created, err)
	}
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(run.RunID, rely("reporting-svc", "report", "rpt_9")); err != nil {
		t.Fatal(err)
	}
	if n := len(f.eventsOf(cert.CertificationID, "dqc.certification.relied-on")); n != 3 {
		t.Fatalf("relied-on events = %d, want one per distinct reliance (3)", n)
	}

	br, err := f.s.GetBlastRadius(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if br.Status != domain.CertificationActive || br.Revocation != nil || br.Count != 3 || len(br.Reliances) != 3 ||
		br.ByService["gl-svc"] != 2 || br.ByService["reporting-svc"] != 1 {
		t.Fatalf("blast radius of an active certification: %+v", br)
	}
}

func TestReliance_RevocationNamesExactlyWhatRestedOnTheCertification(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("reliance-revoke")
	other, otherCert := f.certified("reliance-other")
	for _, r := range []domain.RecordRelianceRequest{rely("gl-svc", "journal-batch", "jb_1"), rely("tax-svc", "filing", "fil_7")} {
		if _, _, err := f.relyOn(run.RunID, r); err != nil {
			t.Fatal(err)
		}
	}
	// Something relied on a DIFFERENT certification: it is not in this blast radius.
	if _, _, err := f.relyOn(other.RunID, rely("gl-svc", "journal-batch", "jb_unrelated")); err != nil {
		t.Fatal(err)
	}
	before, _ := f.s.GetBlastRadius(f.ctx, orgA, run.RunID)

	rev, err := f.revoke(run.RunID, "control-owner", falsePass)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	br, err := f.s.GetBlastRadius(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if br.Status != domain.CertificationRevoked || br.Revocation == nil || br.Revocation.RevocationID != rev.RevocationID || br.Count != 2 {
		t.Fatalf("blast radius after revocation: %+v", br)
	}
	// Prospective: the reliance records are exactly what they were.
	for i := range br.Reliances {
		if br.Reliances[i] != before.Reliances[i] {
			t.Fatalf("revocation changed a reliance record: %+v vs %+v", br.Reliances[i], before.Reliances[i])
		}
	}

	// The revocation event carries the count; each reliance gets its own withdrawal event naming it.
	revoked := f.eventsOf(cert.CertificationID, "dqc.certification.revoked")
	if len(revoked) != 1 || revoked[0]["relying_count"] != float64(2) {
		t.Fatalf("revoked event: %+v", revoked)
	}
	withdrawn := f.eventsOf(cert.CertificationID, "dqc.certification.reliance-withdrawn")
	if len(withdrawn) != 2 {
		t.Fatalf("withdrawal events = %d, want 2", len(withdrawn))
	}
	seen := map[string]bool{}
	for _, e := range withdrawn {
		seen[e["relying_service"].(string)+"/"+e["relying_object_id"].(string)] = true
		if e["revocation_id"] != rev.RevocationID || e["reason_code"] != string(domain.RevokeFalsePass) {
			t.Fatalf("withdrawal event does not name the revocation: %+v", e)
		}
	}
	if !seen["gl-svc/jb_1"] || !seen["tax-svc/fil_7"] || seen["gl-svc/jb_unrelated"] {
		t.Fatalf("withdrawal events named %v", seen)
	}
	if n := len(f.eventsOf(otherCert.CertificationID, "dqc.certification.reliance-withdrawn")); n != 0 {
		t.Fatalf("an unrevoked certification produced %d withdrawal events", n)
	}
}

func TestReliance_RevokingWithNothingRelyingSaysSo(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("reliance-none")
	if _, err := f.revoke(run.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	revoked := f.eventsOf(cert.CertificationID, "dqc.certification.revoked")
	if len(revoked) != 1 || revoked[0]["relying_count"] != float64(0) {
		t.Fatalf("revoked event: %+v", revoked)
	}
	if n := len(f.eventsOf(cert.CertificationID, "dqc.certification.reliance-withdrawn")); n != 0 {
		t.Fatalf("withdrawal events = %d", n)
	}
	if br, err := f.s.GetBlastRadius(f.ctx, orgA, run.RunID); err != nil || br.Count != 0 || br.Reliances == nil || br.ByService == nil {
		t.Fatalf("an empty blast radius must be an empty list, not null: %+v %v", br, err)
	}
}

func TestReliance_CannotBeRecordedOnARevokedCertification(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("reliance-after-revoke")
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke(run.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_after")); !errors.Is(err, domain.ErrRelianceInactive) {
		t.Fatalf("reliance on a revoked certification: %v", err)
	}
	// Not even a repeat of an old reliance is accepted: the answer to "may I rely on this?" is no.
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); !errors.Is(err, domain.ErrRelianceInactive) {
		t.Fatalf("repeat reliance on a revoked certification: %v", err)
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_certification_reliances WHERE certification_id = $1`, cert.CertificationID).Scan(&n)
	if n != 1 {
		t.Fatalf("reliances after a refused attempt: %d, want the original 1", n)
	}
}

func TestReliance_CannotBeRecordedOnAStaleCertification(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "reliance.window", windowed(3600))
	cert := f.certifiedAgainst(c, "pop-reliance-window")
	f.clockAt(cert.ValidUntil.Add(-time.Second))
	if _, _, err := f.relyOn(cert.RunID, rely("gl-svc", "journal-batch", "jb_ok")); err != nil {
		t.Fatalf("reliance inside the window: %v", err)
	}
	f.clockAt(cert.ValidUntil.Add(time.Second))
	if _, _, err := f.relyOn(cert.RunID, rely("gl-svc", "journal-batch", "jb_late")); !errors.Is(err, domain.ErrRelianceInactive) {
		t.Fatalf("reliance past the window: %v", err)
	}
	// What relied on it while it was valid is still its blast radius, and the status says STALE.
	br, err := f.s.GetBlastRadius(f.ctx, orgA, cert.RunID)
	if err != nil || br.Status != domain.CertificationStale || br.Count != 1 {
		t.Fatalf("blast radius of a stale certification: %+v %v", br, err)
	}
}

func TestReliance_RefusesMalformedRequestsAndUnknownCertifications(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("reliance-invalid")
	ok := rely("gl-svc", "journal-batch", "jb_1")
	with := func(mut func(*domain.RecordRelianceRequest)) domain.RecordRelianceRequest { r := ok; mut(&r); return r }
	for name, req := range map[string]domain.RecordRelianceRequest{
		"no service":          with(func(r *domain.RecordRelianceRequest) { r.RelyingService = "" }),
		"service with space":  with(func(r *domain.RecordRelianceRequest) { r.RelyingService = "gl svc" }),
		"no object type":      with(func(r *domain.RecordRelianceRequest) { r.RelyingObjectType = "" }),
		"object id too long":  with(func(r *domain.RecordRelianceRequest) { r.RelyingObjectID = strings.Repeat("a", 129) }),
		"object id injection": with(func(r *domain.RecordRelianceRequest) { r.RelyingObjectID = "x'; DROP TABLE dq_runs;--" }),
		"no purpose":          with(func(r *domain.RecordRelianceRequest) { r.Purpose = "  " }),
		"multi-line purpose":  with(func(r *domain.RecordRelianceRequest) { r.Purpose = "a\nb" }),
		"purpose too long":    with(func(r *domain.RecordRelianceRequest) { r.Purpose = strings.Repeat("p", 501) }),
	} {
		if _, _, err := f.relyOn(run.RunID, req); !errors.Is(err, domain.ErrRelianceInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, _, err := f.s.RecordReliance(f.ctx, orgA, run.RunID, " ", ok); !errors.Is(err, domain.ErrRelianceInvalid) {
		t.Fatalf("blank principal: %v", err)
	}
	if _, _, err := f.relyOn("no-such-run", ok); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := f.s.GetBlastRadius(f.ctx, orgA, "no-such-run"); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("blast radius of an unknown run: %v", err)
	}
}

func TestReliance_IsTenantIsolated(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("reliance-tenant")
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetBlastRadius(f.ctx, orgB, run.RunID); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("another tenant read the blast radius: %v", err)
	}
	if _, _, err := f.s.RecordReliance(f.ctx, orgB, run.RunID, "intruder", rely("x", "y", "z")); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("another tenant recorded a reliance: %v", err)
	}
}

func execErr(f *fixture, stmt string, args ...any) error {
	_, err := f.admin.Exec(f.ctx, stmt, args...)
	return err
}

// The database holds the rules even if the service is bypassed.
func TestReliance_DatabaseHoldsTheRulesToo(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("reliance-db")
	rel, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1"))
	if err != nil {
		t.Fatal(err)
	}
	revokedRun, revokedCert := f.certified("reliance-db-revoked")
	if _, err := f.revoke(revokedRun.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	const cols = `(reliance_id, tenant_id, certification_id, relying_service, relying_object_type, relying_object_id, purpose, recorded_by)`
	ins := func(id, tenant, certID, purpose string) error {
		return execErr(f, `INSERT INTO dq_certification_reliances `+cols+` VALUES ($1, $2, $3, 'gl-svc', 'journal-batch', 'jb_1', $4, 'x')`, id, tenant, certID, purpose)
	}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"update":                       {execErr(f, `UPDATE dq_certification_reliances SET purpose = 'rewritten' WHERE reliance_id = '`+rel.RelianceID+`'`), "immutable"},
		"delete":                       {execErr(f, `DELETE FROM dq_certification_reliances WHERE reliance_id = '`+rel.RelianceID+`'`), "immutable"},
		"on a revoked certification":   {ins("dqr_00000000-0000-0000-0000-000000000001", orgA, revokedCert.CertificationID, "p"), "revoked"},
		"under another tenant":         {ins("dqr_00000000-0000-0000-0000-000000000002", orgB, cert.CertificationID, "p"), "does not belong to this tenant"},
		"duplicate of an existing one": {ins("dqr_00000000-0000-0000-0000-000000000003", orgA, cert.CertificationID, "p"), "duplicate"},
		"a malformed id":               {ins("nope", orgA, cert.CertificationID, "p"), "reliance_id_check"},
		"a blank purpose":              {ins("dqr_00000000-0000-0000-0000-000000000004", orgA, cert.CertificationID, " "), "purpose"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, tc.err, tc.want)
		}
	}

	// Past its validity window the database refuses too, whatever clock the service used.
	f2 := newContractFixture(t)
	c := f2.published(orgA, "reliance.db.window", windowed(60))
	wc := f2.certifiedAgainst(c, "pop-reliance-db-window")
	err = execErr(f2, `INSERT INTO dq_certification_reliances (reliance_id, tenant_id, certification_id, relying_service,
			relying_object_type, relying_object_id, purpose, recorded_by, recorded_at)
		VALUES ('dqr_00000000-0000-0000-0000-000000000005', $1, $2, 's', 't', 'i', 'p', 'x', $3)`, orgA, wc.CertificationID, wc.ValidUntil.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "validity window") {
		t.Fatalf("a reliance after the validity window: %v", err)
	}
}

// The blast radius a revocation names is COMPLETE: reliances racing a revocation either land before it (and are named
// by it) or are refused after it. None can slip in unnamed.
func TestReliance_ConcurrentWithRevocationNeverEscapesTheBlastRadius(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("reliance-race")
	const workers = 24
	start := make(chan struct{})
	done := make(chan error, workers+1)
	for i := 0; i < workers; i++ {
		go func(i int) {
			<-start
			_, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_race_"+string(rune('a'+i))))
			if errors.Is(err, domain.ErrRelianceInactive) {
				err = nil // refused after the revocation: the right answer
			}
			done <- err
		}(i)
	}
	go func() {
		<-start
		_, err := f.revoke(run.RunID, "control-owner", falsePass)
		done <- err
	}()
	close(start)
	for i := 0; i < workers+1; i++ {
		if err := <-done; err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
	var recorded int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_certification_reliances WHERE certification_id = $1`, cert.CertificationID).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	revoked := f.eventsOf(cert.CertificationID, "dqc.certification.revoked")
	withdrawn := f.eventsOf(cert.CertificationID, "dqc.certification.reliance-withdrawn")
	if len(revoked) != 1 || int(revoked[0]["relying_count"].(float64)) != recorded || len(withdrawn) != recorded {
		t.Fatalf("recorded %d reliances; the revocation named %v and emitted %d withdrawal events", recorded, revoked[0]["relying_count"], len(withdrawn))
	}
	// Commit order, not timestamps (revoked_at is the revoking transaction's start, recorded_at the service clock): every
	// reliance must have been inserted by a transaction that began before the one that revoked.
	var after int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_certification_reliances l JOIN dq_certification_revocations v USING (certification_id)
		WHERE l.xmin::text::bigint > v.xmin::text::bigint`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("%d reliances were inserted by a transaction after the revoking one", after)
	}
}

// 000011's down refuses while any reliance is recorded, and round-trips while none is.
func TestMigration000011_DownRefusesWhileRelianceExists(t *testing.T) {
	f := newFixture(t)
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	down, err := os.ReadFile(filepath.Join(dir, "000011_certification_reliance.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(dir, "000011_certification_reliance.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	run, _ := f.certified("reliance-migration")
	if _, err := f.admin.Exec(f.ctx, string(down)); err != nil {
		t.Fatalf("down with no reliance: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON dq_certification_reliances TO zoiko_app_test`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatalf("reliance after the round trip: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(down)); err == nil || !strings.Contains(err.Error(), "000011 down refused") {
		t.Fatalf("down with a reliance: %v", err)
	}
	var n int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM dq_certification_reliances`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("a refused down changed the reliances: %d, %v", n, err)
	}
}
