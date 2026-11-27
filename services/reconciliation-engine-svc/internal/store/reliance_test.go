package store_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "blast radius of a revocation"): what relied on a
// reconciliation certification is recorded, and revoking it names exactly that set.

const (
	typeRelianceRecorded  = "com.zoikosuite.accounting.reconciliation-certification.reliance-recorded"
	typeRelianceWithdrawn = "com.zoikosuite.accounting.reconciliation-certification.reliance-withdrawn"
	typeCertRevoked       = "com.zoikosuite.accounting.reconciliation-certification.revoked"
)

func rely(service, typ, id string) domain.RecordRelianceRequest {
	return domain.RecordRelianceRequest{RelyingService: service, RelyingObjectType: typ, RelyingObjectID: id, Purpose: "built on the certified reconciliation"}
}

func (f *fixture) relyOn(runID string, req domain.RecordRelianceRequest) (*domain.CertificationReliance, bool, error) {
	return f.s.RecordReliance(f.ctx, orgA, runID, "downstream-svc", req)
}

func (f *fixture) revokeRun(runID string) (*domain.CertificationRevocation, error) {
	return f.s.RevokeCertification(f.ctx, orgA, runID, revokeRequest(), "controller-erin", grant("controller-erin"), f.claim("RevokeCertification", runID))
}

// eventData is the data member of every outbox event of one type about one certification.
func (f *fixture) eventData(eventType, certificationID string) []map[string]any {
	f.t.Helper()
	var out []map[string]any
	for _, r := range f.outboxRows() {
		if r.eventType != eventType || r.aggregateID == nil || *r.aggregateID != certificationID {
			continue
		}
		data, _ := r.payload["data"].(map[string]any)
		if data == nil {
			f.t.Fatalf("event %s has no data member: %v", eventType, r.payload)
		}
		out = append(out, data)
	}
	return out
}

func (f *fixture) certID(runID string) string {
	f.t.Helper()
	c, err := f.s.GetCertification(f.ctx, orgA, runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return c.CertificationID
}

func TestReliance_RecordedIdempotentlyAndListedAsTheBlastRadius(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-basic")
	cid := f.certID(run.RunID)

	a, created, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1"))
	if err != nil || !created || a.CertificationID != cid || a.RunID != run.RunID || a.RecordedBy != "downstream-svc" {
		t.Fatalf("first reliance: %+v created=%v err=%v", a, created, err)
	}
	again, created, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1"))
	if err != nil || created || again.RelianceID != a.RelianceID {
		t.Fatalf("repeat: %+v created=%v err=%v", again, created, err)
	}
	for _, r := range []domain.RecordRelianceRequest{rely("gl-svc", "journal-batch", "jb_2"), rely("reporting-svc", "report", "rpt_9")} {
		if _, _, err := f.relyOn(run.RunID, r); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(f.eventData(typeRelianceRecorded, cid)); n != 3 {
		t.Fatalf("reliance-recorded events = %d, want one per distinct reliance (3)", n)
	}

	br, err := f.s.GetBlastRadius(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if br.CertificationState != domain.CertCertified || br.Revocation != nil || br.Count != 3 || len(br.Reliances) != 3 ||
		br.ByService["gl-svc"] != 2 || br.ByService["reporting-svc"] != 1 {
		t.Fatalf("blast radius of a certified run: %+v", br)
	}
}

func TestReliance_EventsAreCanonicalEnvelopesAboutTheCertification(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-envelope")
	cid := f.certID(run.RunID)
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	var got *outboxRow
	rows := f.outboxRows()
	for i := range rows {
		if rows[i].eventType == typeRelianceRecorded {
			got = &rows[i]
		}
	}
	if got == nil || got.tenantID != orgA || got.payload["legal_entity_id"] != entityA || got.payload["actor_id"] != "downstream-svc" ||
		got.partitionKey != expectedPartitionKey(orgA, "reconciliation-certification", cid) {
		t.Fatalf("reliance-recorded event: %+v", got)
	}
}

func TestReliance_RevocationNamesExactlyWhatRestedOnTheCertification(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-revoke")
	cid := f.certID(run.RunID)
	other := f.certifiedRun("reliance-other")
	for _, r := range []domain.RecordRelianceRequest{rely("gl-svc", "journal-batch", "jb_1"), rely("tax-svc", "filing", "fil_7")} {
		if _, _, err := f.relyOn(run.RunID, r); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.relyOn(other.RunID, rely("gl-svc", "journal-batch", "jb_unrelated")); err != nil {
		t.Fatal(err)
	}
	before, _ := f.s.GetBlastRadius(f.ctx, orgA, run.RunID)

	rev, err := f.revokeRun(run.RunID)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	br, err := f.s.GetBlastRadius(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if br.CertificationState != domain.CertRevoked || br.Revocation == nil || br.Revocation.RevocationID != rev.RevocationID || br.Count != 2 {
		t.Fatalf("blast radius after revocation: %+v", br)
	}
	for i := range br.Reliances {
		if br.Reliances[i] != before.Reliances[i] {
			t.Fatalf("revocation changed a reliance record: %+v vs %+v", br.Reliances[i], before.Reliances[i])
		}
	}

	revoked := f.eventData(typeCertRevoked, cid)
	if len(revoked) != 1 || revoked[0]["relying_count"] != float64(2) || revoked[0]["revocation_id"] != rev.RevocationID {
		t.Fatalf("revoked event: %+v", revoked)
	}
	withdrawn := f.eventData(typeRelianceWithdrawn, cid)
	if len(withdrawn) != 2 {
		t.Fatalf("withdrawal events = %d, want 2", len(withdrawn))
	}
	seen := map[string]bool{}
	for _, e := range withdrawn {
		seen[e["relying_service"].(string)+"/"+e["relying_object_id"].(string)] = true
		if e["revocation_id"] != rev.RevocationID {
			t.Fatalf("withdrawal event does not name the revocation: %+v", e)
		}
	}
	if !seen["gl-svc/jb_1"] || !seen["tax-svc/fil_7"] || seen["gl-svc/jb_unrelated"] {
		t.Fatalf("withdrawal events named %v", seen)
	}
	if n := len(f.eventData(typeRelianceWithdrawn, f.certID(other.RunID))); n != 0 {
		t.Fatalf("an unrevoked certification produced %d withdrawal events", n)
	}
}

func TestReliance_RevokingWithNothingRelyingSaysSo(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-none")
	cid := f.certID(run.RunID)
	if _, err := f.revokeRun(run.RunID); err != nil {
		t.Fatal(err)
	}
	revoked := f.eventData(typeCertRevoked, cid)
	if len(revoked) != 1 || revoked[0]["relying_count"] != float64(0) {
		t.Fatalf("revoked event: %+v", revoked)
	}
	if n := len(f.eventData(typeRelianceWithdrawn, cid)); n != 0 {
		t.Fatalf("withdrawal events = %d", n)
	}
	if br, err := f.s.GetBlastRadius(f.ctx, orgA, run.RunID); err != nil || br.Count != 0 || br.Reliances == nil || br.ByService == nil {
		t.Fatalf("an empty blast radius must be an empty list, not null: %+v %v", br, err)
	}
}

func TestReliance_CannotBeRecordedOnARevokedSupersededOrUncertifiedRun(t *testing.T) {
	f := newFixture(t)

	revoked := f.certifiedRun("reliance-after-revoke")
	if _, _, err := f.relyOn(revoked.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.revokeRun(revoked.RunID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"jb_after", "jb_1"} { // a new one, and a repeat of an old one
		if _, _, err := f.relyOn(revoked.RunID, rely("gl-svc", "journal-batch", id)); !errors.Is(err, domain.ErrRelianceInactive) {
			t.Fatalf("reliance %s on a revoked certification: %v", id, err)
		}
	}
	if n := f.count("reconciliation_certification_reliances"); n != 1 {
		t.Fatalf("reliances after refused attempts: %d, want the original 1", n)
	}

	// A superseded run's certification was replaced by a corrected one: reliance goes to the replacement.
	def := f.defineReconciliation(orgA, "reliance-superseded", 0)
	oldRun := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(oldRun.RunID)
	newRun := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(newRun.RunID)
	if _, err := f.s.SupersedeRun(f.ctx, orgA, oldRun.RunID, domain.SupersedeRunRequest{NewRunID: newRun.RunID, Reason: "corrected"}, "controller-carol",
		f.claim("Supersede", oldRun.RunID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(oldRun.RunID, rely("gl-svc", "journal-batch", "jb_old")); !errors.Is(err, domain.ErrRelianceInactive) {
		t.Fatalf("reliance on a superseded certification: %v", err)
	}
	if _, _, err := f.relyOn(newRun.RunID, rely("gl-svc", "journal-batch", "jb_new")); err != nil {
		t.Fatalf("reliance on the replacement: %v", err)
	}

	// A run that was never certified has nothing to rely on.
	open := f.startRun(orgA, def, cleanPops())
	if _, _, err := f.relyOn(open.RunID, rely("gl-svc", "journal-batch", "jb_none")); !errors.Is(err, domain.ErrRelianceInactive) {
		t.Fatalf("reliance on an uncertified run: %v", err)
	}
}

func TestReliance_RefusesMalformedRequestsAndUnknownRuns(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-invalid")
	ok := rely("gl-svc", "journal-batch", "jb_1")
	with := func(mut func(*domain.RecordRelianceRequest)) domain.RecordRelianceRequest { r := ok; mut(&r); return r }
	for name, req := range map[string]domain.RecordRelianceRequest{
		"no service":          with(func(r *domain.RecordRelianceRequest) { r.RelyingService = "" }),
		"service with space":  with(func(r *domain.RecordRelianceRequest) { r.RelyingService = "gl svc" }),
		"no object type":      with(func(r *domain.RecordRelianceRequest) { r.RelyingObjectType = "" }),
		"object id too long":  with(func(r *domain.RecordRelianceRequest) { r.RelyingObjectID = strings.Repeat("a", 129) }),
		"object id injection": with(func(r *domain.RecordRelianceRequest) { r.RelyingObjectID = "x'; DROP TABLE certifications;--" }),
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
	if _, _, err := f.relyOn("rcr_00000000-0000-0000-0000-000000000000", ok); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := f.s.GetBlastRadius(f.ctx, orgA, "rcr_00000000-0000-0000-0000-000000000000"); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("blast radius of an unknown run: %v", err)
	}
}

func TestReliance_IsTenantIsolated(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-tenant")
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetBlastRadius(f.ctx, orgB, run.RunID); !errors.Is(err, domain.ErrRunNotFound) {
		t.Fatalf("another tenant read the blast radius: %v", err)
	}
	if _, _, err := f.s.RecordReliance(f.ctx, orgB, run.RunID, "intruder", rely("x", "y", "z")); !errors.Is(err, domain.ErrRunNotFound) {
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
	run := f.certifiedRun("reliance-db")
	cid := f.certID(run.RunID)
	rel, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1"))
	if err != nil {
		t.Fatal(err)
	}
	revokedRun := f.certifiedRun("reliance-db-revoked")
	revokedCID := f.certID(revokedRun.RunID)
	if _, err := f.revokeRun(revokedRun.RunID); err != nil {
		t.Fatal(err)
	}
	const cols = `(reliance_id, tenant_id, certification_id, relying_service, relying_object_type, relying_object_id, purpose, recorded_by)`
	ins := func(id, tenant, certID, purpose string) error {
		return execErr(f, `INSERT INTO reconciliation_certification_reliances `+cols+` VALUES ($1, $2, $3, 'gl-svc', 'journal-batch', 'jb_1', $4, 'x')`, id, tenant, certID, purpose)
	}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"update":                       {execErr(f, `UPDATE reconciliation_certification_reliances SET purpose = 'rewritten' WHERE reliance_id = '`+rel.RelianceID+`'`), "immutable"},
		"delete":                       {execErr(f, `DELETE FROM reconciliation_certification_reliances WHERE reliance_id = '`+rel.RelianceID+`'`), "immutable"},
		"on a revoked certification":   {ins("rrl_00000000-0000-0000-0000-000000000001", orgA, revokedCID, "p"), "REVOKED"},
		"under another tenant":         {ins("rrl_00000000-0000-0000-0000-000000000002", orgB, cid, "p"), "does not belong to this tenant"},
		"duplicate of an existing one": {ins("rrl_00000000-0000-0000-0000-000000000003", orgA, cid, "p"), "duplicate"},
		"a malformed id":               {ins("nope", orgA, cid, "p"), "reliance_id_check"},
		"a blank purpose":              {ins("rrl_00000000-0000-0000-0000-000000000004", orgA, cid, " "), "purpose"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Fatalf("%s: %v, want a refusal mentioning %q", name, tc.err, tc.want)
		}
	}
}

// The blast radius a revocation names is COMPLETE: reliances racing a revocation either land before it (and are named
// by it) or are refused after it. None can slip in unnamed.
func TestReliance_ConcurrentWithRevocationNeverEscapesTheBlastRadius(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("reliance-race")
	cid := f.certID(run.RunID)
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
		_, err := f.revokeRun(run.RunID)
		done <- err
	}()
	close(start)
	for i := 0; i < workers+1; i++ {
		if err := <-done; err != nil {
			t.Fatalf("worker: %v", err)
		}
	}
	recorded := f.count("reconciliation_certification_reliances")
	revoked := f.eventData(typeCertRevoked, cid)
	withdrawn := f.eventData(typeRelianceWithdrawn, cid)
	if len(revoked) != 1 || int(revoked[0]["relying_count"].(float64)) != recorded || len(withdrawn) != recorded {
		t.Fatalf("recorded %d reliances; the revocation named %v and emitted %d withdrawal events", recorded, revoked[0]["relying_count"], len(withdrawn))
	}
	var after int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM reconciliation_certification_reliances l JOIN reconciliation_certification_revocations v
		ON v.certification_id = l.certification_id WHERE l.xmin::text::bigint > v.xmin::text::bigint`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("%d reliances were inserted by a transaction after the revoking one", after)
	}
}

// 000017's down refuses while any reliance is recorded, and round-trips while none is.
func TestMigration000017_DownRefusesWhileRelianceExists(t *testing.T) {
	f := newFixture(t)
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	down, err := os.ReadFile(filepath.Join(dir, "000017_certification_reliance.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile(filepath.Join(dir, "000017_certification_reliance.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	run := f.certifiedRun("reliance-migration")
	if _, err := f.admin.Exec(f.ctx, string(down)); err != nil {
		t.Fatalf("down with no reliance: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(up)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, `GRANT SELECT, INSERT, UPDATE, DELETE ON reconciliation_certification_reliances TO zoiko_app_test`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatalf("reliance after the round trip: %v", err)
	}
	if _, err := f.admin.Exec(f.ctx, string(down)); err == nil || !strings.Contains(err.Error(), "000017 down refused") {
		t.Fatalf("down with a reliance: %v", err)
	}
	if n := f.count("reconciliation_certification_reliances"); n != 1 {
		t.Fatalf("a refused down changed the reliances: %d", n)
	}
}
