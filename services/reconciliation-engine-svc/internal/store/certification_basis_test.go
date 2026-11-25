package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
	"zoiko.io/reconciliation-engine-svc/internal/store"
)

// ZS-SVC-Z-001 DQC-05 / TC-13 / NP-43 / section 11 (docs/architecture/DQC_GAP_MATRIX.md, step C).
//
// Before this change a certification never went stale, had no identity
// independent of its run, and rested on no stated lineage.

const goodLineageSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func lineageRef(id string) domain.CertifyRequest {
	sha := goodLineageSHA
	return domain.CertifyRequest{LineageManifestID: &id, LineageManifestSHA256: &sha}
}

func int64p(n int64) *int64 { return &n }

// defineWithWindow defines an approved reconciliation whose pinned certification
// profile holds certification to a freshness window of seconds (nil = none).
func (f *fixture) defineWithWindow(name string, seconds *int64) *domain.ReconciliationDefinition {
	f.t.Helper()
	req := defineRequest(name, 0)
	req.CertificationProfile = domain.CertificationProfile{MaxEvidenceAgeSeconds: seconds}
	return f.defineApproved(orgA, req)
}

// storeAt is a store whose clock reads now+offset: certification freshness and
// the seal time both follow it.
func (f *fixture) storeAt(offset time.Duration) *store.PgStore {
	return store.NewPgStore(f.app, store.WithEventRegion(testEventRegion),
		store.WithClock(func() time.Time { return time.Now().UTC().Add(offset) }))
}

func (f *fixture) certifyOn(s *store.PgStore, runID string, req domain.CertifyRequest) (*domain.Certification, error) {
	f.t.Helper()
	return s.CertifyWithEvidence(f.ctx, orgA, runID, "certifier", grant("certifier"), req, f.claim("Certify", runID))
}

// matchedRun starts a run over pops and matches it clean, ready to certify.
func (f *fixture) matchedRun(def *domain.ReconciliationDefinition, pops []testPop) *domain.ReconciliationRun {
	f.t.Helper()
	run := f.startRun(orgA, def, pops)
	f.match(run.RunID)
	return run
}

// NP-43: stale evidence is not certified, and nothing is sealed by the refusal.
func TestCertify_EvidenceOlderThanTheProfileWindow_IsRefused(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("fresh-window", int64p(3600))
	run := f.matchedRun(def, cleanPops())

	_, err := f.certifyOn(f.storeAt(2*time.Hour), run.RunID, domain.CertifyRequest{})
	if !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("err = %v, want ErrEvidenceStale (1h window, evidence 2h old)", err)
	}
	if got, _ := f.s.GetRun(f.ctx, orgA, run.RunID); got.Status == domain.RunCertified {
		t.Fatal("a refused certification certified the run")
	}
	// Still certifiable while the evidence is within the window.
	if _, err := f.certifyOn(f.s, run.RunID, domain.CertifyRequest{}); err != nil {
		t.Fatalf("certify within the window: %v", err)
	}
}

// A profile that declares no window keeps today's behaviour: evidence does not
// expire. (The window is a profile decision, pinned and versioned with the rules.)
func TestCertify_AProfileWithoutAWindow_DoesNotExpire(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("fresh-none", nil)
	run := f.matchedRun(def, cleanPops())
	if _, err := f.certifyOn(f.storeAt(90*24*time.Hour), run.RunID, domain.CertifyRequest{}); err != nil {
		t.Fatalf("a profile with no window should not expire evidence: %v", err)
	}
}

// A window that is zero or negative is a mistake, not "no window".
func TestDefine_NonPositiveFreshnessWindow_IsRefused(t *testing.T) {
	f := newFixture(t)
	for _, n := range []int64{0, -60} {
		req := defineRequest("fresh-bad-window", 0)
		req.CertificationProfile = domain.CertificationProfile{MaxEvidenceAgeSeconds: int64p(n)}
		_, err := f.s.DefineReconciliation(f.ctx, orgA, req, "test-operator", f.claim("DefineReconciliation", "bad"))
		if !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("window %d: err = %v, want ErrInvalidArgument", n, err)
		}
	}
}

// TC-13: the certification seals the freshness state it was issued under, and
// each certified population records when it was frozen.
func TestCertify_SealsFreshnessStateAndBasis(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("fresh-sealed", int64p(7200))
	run := f.matchedRun(def, cleanPops())

	cert, err := f.certifyOn(f.s, run.RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if len(cert.BasisSHA256) != 64 {
		t.Fatalf("basis = %q", cert.BasisSHA256)
	}
	if cert.EvidenceMaxAgeSeconds == nil || *cert.EvidenceMaxAgeSeconds != 7200 {
		t.Fatalf("window sealed as %v", cert.EvidenceMaxAgeSeconds)
	}
	if cert.EvidenceOldestAt == nil {
		t.Fatal("oldest evidence time not sealed")
	}
	oldest := cert.Populations[0].FrozenAt
	for _, p := range cert.Populations {
		if p.FrozenAt.IsZero() {
			t.Fatalf("population %s has no frozen_at", p.Side)
		}
		if p.FrozenAt.Before(oldest) {
			oldest = p.FrozenAt
		}
	}
	if !cert.EvidenceOldestAt.Equal(oldest) {
		t.Fatalf("sealed oldest = %v, populations say %v", cert.EvidenceOldestAt, oldest)
	}
	got, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil || got.BasisSHA256 != cert.BasisSHA256 || got.EvidenceMaxAgeSeconds == nil {
		t.Fatalf("GetCertification did not return the sealed basis: %+v (err %v)", got, err)
	}
}

// section 11: the same evidence is one basis, whichever run it is certified under.
func TestCertify_TheSameBasisTwice_IsRefused_AndNamesTheExistingOne(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-dup", nil)
	pops := cleanPops() // identical records, so an identical content hash on both runs

	first := f.matchedRun(def, pops)
	cert1, err := f.certifyOn(f.s, first.RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("first certify: %v", err)
	}
	second := f.matchedRun(def, pops)
	if second.RunID == first.RunID {
		t.Fatal("expected a distinct run")
	}
	_, err = f.certifyOn(f.s, second.RunID, domain.CertifyRequest{})
	if !errors.Is(err, domain.ErrCertificationBasisExists) {
		t.Fatalf("err = %v, want ErrCertificationBasisExists", err)
	}
	if !strings.Contains(err.Error(), cert1.CertificationID) {
		t.Fatalf("the refusal should name the existing certification %s: %v", cert1.CertificationID, err)
	}
	if got, _ := f.s.GetRun(f.ctx, orgA, second.RunID); got.Status == domain.RunCertified {
		t.Fatal("a refused certification certified the run")
	}
}

// The basis ignores the run, the certifier and the time, but not the evidence:
// different frozen content is a different basis.
func TestCertify_DifferentEvidence_IsADifferentBasis(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-diff", nil)
	c1, err := f.certifyOn(f.s, f.matchedRun(def, cleanPops()).RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	c2, err := f.certifyOn(f.s, f.matchedRun(def, cleanPops()).RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("different evidence should certify: %v", err)
	}
	if c1.BasisSHA256 == c2.BasisSHA256 {
		t.Fatal("different frozen content produced the same basis")
	}
}

// The database enforces it too, for any other writer.
func TestDatabase_RefusesTwoCertificationsOfOneBasis(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-db", nil)
	cert, err := f.certifyOn(f.s, f.matchedRun(def, cleanPops()).RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	other := f.startRun(orgA, def, cleanPops())
	// A copy of the real certification with a new id, run and basis: every other
	// column is already valid, so only the basis can be what the database refuses.
	ins := func(id, basis string) error {
		_, err := f.admin.Exec(f.ctx, `
			INSERT INTO certifications (certification_id, tenant_id, run_id, definition_version, matched_count, exception_count,
				total_asserted_minor_units, control_totals, result_state, assessment, authority_snapshot, populations, manual_matches,
				manifest_sha256, sealed_at, sealed_by, basis_sha256, lineage_manifest_id, lineage_manifest_sha256,
				evidence_oldest_at, evidence_max_age_seconds)
			SELECT $2, tenant_id, $3, definition_version, matched_count, exception_count,
				total_asserted_minor_units, control_totals, result_state, assessment, authority_snapshot, populations, manual_matches,
				manifest_sha256, sealed_at, sealed_by, $4, lineage_manifest_id, lineage_manifest_sha256,
				evidence_oldest_at, evidence_max_age_seconds
			FROM certifications WHERE certification_id = $1`,
			cert.CertificationID, id, other.RunID, basis)
		return err
	}
	if err := ins("rct_00000000-0000-0000-0000-0000000000f1", cert.BasisSHA256); err == nil {
		t.Fatal("database accepted a second certification with the same basis_sha256")
	}
	// Prove it is the uniqueness that refused it, not a malformed row.
	if err := ins("rct_00000000-0000-0000-0000-0000000000f2", strings.Repeat("b", 64)); err != nil {
		t.Fatalf("database refused a well-formed certification: %v", err)
	}
}

// DQC-05: the lineage manifest a certification rests on is part of its basis.
func TestCertify_LineageManifestReference_IsBoundIntoTheBasis(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-lineage", nil)
	pops := cleanPops()
	idA, idB := "dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "dpm_bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	a, err := f.certifyOn(f.s, f.matchedRun(def, pops).RunID, lineageRef(idA))
	if err != nil {
		t.Fatalf("certify with manifest A: %v", err)
	}
	if a.LineageManifestID == nil || *a.LineageManifestID != idA || a.LineageManifestSHA256 == nil {
		t.Fatalf("lineage reference not recorded: %+v", a)
	}
	// Identical evidence over a DIFFERENT lineage is a different basis...
	b, err := f.certifyOn(f.s, f.matchedRun(def, pops).RunID, lineageRef(idB))
	if err != nil {
		t.Fatalf("identical evidence over manifest B should be its own basis: %v", err)
	}
	// ...and so is the same evidence with none.
	c, err := f.certifyOn(f.s, f.matchedRun(def, pops).RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("no-reference certification: %v", err)
	}
	if a.BasisSHA256 == b.BasisSHA256 || a.BasisSHA256 == c.BasisSHA256 || b.BasisSHA256 == c.BasisSHA256 {
		t.Fatal("the lineage reference did not change the basis")
	}
}

func TestCertify_MalformedLineageReference_IsRefused(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-lineage-bad", nil)
	run := f.matchedRun(def, cleanPops())
	id, sha := "dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", goodLineageSHA
	badID, badSHA := "not-a-manifest", "ABC"

	for name, req := range map[string]domain.CertifyRequest{
		"id without sha":   {LineageManifestID: &id},
		"sha without id":   {LineageManifestSHA256: &sha},
		"malformed id":     {LineageManifestID: &badID, LineageManifestSHA256: &sha},
		"malformed sha256": {LineageManifestID: &id, LineageManifestSHA256: &badSHA},
	} {
		if _, err := f.certifyOn(f.s, run.RunID, req); !errors.Is(err, domain.ErrInvalidArgument) {
			t.Errorf("%s: err = %v, want ErrInvalidArgument", name, err)
		}
	}
	if _, err := f.certifyOn(f.s, run.RunID, domain.CertifyRequest{}); err != nil {
		t.Fatalf("run should still certify after refused attempts: %v", err)
	}
}

// Certify (no evidence argument) is CertifyWithEvidence with none: existing
// callers get a certification with a basis too.
func TestCertify_TheOriginalEntryPoint_StillBindsABasis(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-compat", nil)
	run := f.matchedRun(def, cleanPops())
	cert, err := f.s.Certify(f.ctx, orgA, run.RunID, "certifier", grant("certifier"), f.claim("Certify", run.RunID))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if len(cert.BasisSHA256) != 64 || cert.LineageManifestID != nil {
		t.Fatalf("basis / lineage: %+v", cert)
	}
}

// A basis with no evidence time would read as evidenced while proving nothing;
// the database refuses it. (Copied from a real certification so the CHECK is the
// only thing that can be refusing.)
func TestDatabase_RefusesABasisWithNoEvidenceTime(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("basis-incomplete", nil)
	cert, err := f.certifyOn(f.s, f.matchedRun(def, cleanPops()).RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	other := f.startRun(orgA, def, cleanPops())
	cols := `tenant_id, definition_version, matched_count, exception_count, total_asserted_minor_units, control_totals,
		result_state, assessment, authority_snapshot, populations, manual_matches, manifest_sha256, sealed_at, sealed_by`
	insert := func(id, oldest string) error {
		_, err := f.admin.Exec(f.ctx, `
			INSERT INTO certifications (certification_id, run_id, `+cols+`, basis_sha256, evidence_oldest_at)
			SELECT $2, $3, `+cols+`, $4, `+oldest+` FROM certifications WHERE certification_id = $1`,
			cert.CertificationID, id, other.RunID, strings.Repeat("c", 64))
		return err
	}
	if err := insert("rct_00000000-0000-0000-0000-0000000000e1", "NULL"); err == nil {
		t.Fatal("database accepted a basis with no evidence_oldest_at")
	}
	if err := insert("rct_00000000-0000-0000-0000-0000000000e2", "now()"); err != nil {
		t.Fatalf("database refused a well-formed certification: %v", err)
	}
}
