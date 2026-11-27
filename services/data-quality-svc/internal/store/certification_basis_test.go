package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/store"
)

// ZS-SVC-Z-001 DQC-05 / TC-13 / NP-43 / section 11 (docs/architecture/DQC_GAP_MATRIX.md, step C).
//
// Before this change a certification never went stale, bound nothing about WHEN
// its evidence was observed, and had no identity independent of the run.

const goodLineageSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func lineageRef(id string) domain.CertifyDQRequest {
	sha := goodLineageSHA
	return domain.CertifyDQRequest{LineageManifestID: &id, LineageManifestSHA256: &sha}
}

// runAt records a clean one-rule run whose evidence was observed at asOf (nil =
// "when this service recorded it").
func (f *fixture) runAt(version *domain.DQRuleSetVersion, ref string, asOf *time.Time) *domain.DQRun {
	f.t.Helper()
	run, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: version.VersionID, PopulationRef: ref, PopulationRowCount: 1000,
		PopulationContentHash: hashLike(ref), EvidenceAsOf: asOf,
		RuleOutcomes: []domain.RuleOutcome{passingOutcome("r1")},
	}, "test-operator", f.claim("RunDQ", ref))
	if err != nil {
		f.t.Fatalf("run %s: %v", ref, err)
	}
	if run.Status != domain.RunRunning {
		f.t.Fatalf("run %s = %s, want a clean Running run", ref, run.Status)
	}
	return run
}

func (f *fixture) certifyWith(s *store.PgStore, runID string, req domain.CertifyDQRequest) (*domain.DQCertification, error) {
	f.t.Helper()
	return s.CertifyDQ(f.ctx, orgA, runID, "test-operator", req, f.claim("CertifyDQ", runID))
}

func oneRule(f *fixture, name string) *domain.DQRuleSetVersion {
	return f.publishRuleSet(orgA, name, []domain.RuleDefinition{{RuleKey: "r1", Description: "x"}})
}

func tptr(t time.Time) *time.Time { return &t }

// NP-43: expired evidence is never certified — whether its age comes from the
// caller-asserted observation time...
func TestCertify_EvidenceOlderThanTheWindow_IsRefused_ByAssertedObservationTime(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "fresh-asserted")
	run := f.runAt(v, "pop-stale-asserted", tptr(time.Now().Add(-48*time.Hour)))

	_, err := f.certifyWith(f.s, run.RunID, domain.CertifyDQRequest{})
	if !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("err = %v, want ErrEvidenceStale (default window is 24h, evidence is 48h old)", err)
	}
}

// ...or, failing that, from when the service recorded the run.
func TestCertify_EvidenceOlderThanTheWindow_IsRefused_ByRecordedTime(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "fresh-recorded")
	run := f.runAt(v, "pop-stale-recorded", nil)

	later := store.NewPgStore(f.app).WithClock(func() time.Time { return time.Now().Add(25 * time.Hour) })
	if _, err := f.certifyWith(later, run.RunID, domain.CertifyDQRequest{}); !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("err = %v, want ErrEvidenceStale 25h after the run was recorded", err)
	}
	// ...and nothing was sealed: the run is still certifiable once evidence is current.
	if got, _ := f.s.GetRun(f.ctx, orgA, run.RunID); got.Status != domain.RunRunning {
		t.Fatalf("a refused certification changed the run: %s", got.Status)
	}
	if _, err := f.certifyWith(f.s, run.RunID, domain.CertifyDQRequest{}); err != nil {
		t.Fatalf("certify while the evidence is still fresh: %v", err)
	}
}

// The window belongs to the service, not the caller: there is no request field
// that can widen it, and an unconfigured service still has one.
func TestCertify_TheWindowIsTheServicesNotTheCallers(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "fresh-window")
	run := f.runAt(v, "pop-window", tptr(time.Now().Add(-2*time.Hour)))

	tight := store.NewPgStore(f.app).WithEvidenceMaxAge(time.Hour)
	if _, err := f.certifyWith(tight, run.RunID, domain.CertifyDQRequest{}); !errors.Is(err, domain.ErrEvidenceStale) {
		t.Fatalf("1h window, 2h-old evidence: err = %v, want ErrEvidenceStale", err)
	}
	// A non-positive setting must not turn the check off.
	unlimited := store.NewPgStore(f.app).WithEvidenceMaxAge(0)
	if _, err := f.certifyWith(unlimited, run.RunID, domain.CertifyDQRequest{}); err != nil {
		t.Fatalf("0 falls back to the 24h default, and 2h-old evidence passes it: %v", err)
	}
}

// Evidence cannot have been observed in the future.
func TestRun_ObservationTimeInTheFuture_IsRefused(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "fresh-future")
	_, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{
		RuleSetVersionID: v.VersionID, PopulationRef: "pop-future", PopulationRowCount: 10, PopulationContentHash: hashLike("f"),
		EvidenceAsOf: tptr(time.Now().Add(2 * time.Hour)), RuleOutcomes: []domain.RuleOutcome{passingOutcome("r1")},
	}, "test-operator", f.claim("RunDQ", "pop-future"))
	if err == nil || !strings.Contains(err.Error(), "future") {
		t.Fatalf("err = %v, want a refusal of a future evidence_as_of", err)
	}
}

// TC-13: the certification seals the freshness state it was issued under.
func TestCertify_SealsFreshnessStateAndBasis(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "fresh-sealed")
	asOf := time.Now().Add(-90 * time.Minute).UTC().Truncate(time.Microsecond)
	run := f.runAt(v, "pop-sealed", &asOf)

	cert, err := f.certifyWith(f.s, run.RunID, domain.CertifyDQRequest{})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	s := cert.Summary
	if len(cert.BasisSHA256) != 64 || s.BasisSHA256 != cert.BasisSHA256 {
		t.Fatalf("basis not sealed consistently: cert=%q summary=%q", cert.BasisSHA256, s.BasisSHA256)
	}
	if s.EvidenceAsOf == nil || !s.EvidenceAsOf.Equal(asOf) {
		t.Fatalf("evidence_as_of sealed as %v, want %v", s.EvidenceAsOf, asOf)
	}
	if s.EvidenceMaxAgeSeconds != int64(store.DefaultEvidenceMaxAge/time.Second) {
		t.Fatalf("window sealed as %ds", s.EvidenceMaxAgeSeconds)
	}
	if s.CertifiedAgainstAge < 5300 || s.CertifiedAgainstAge > 5600 {
		t.Fatalf("age at certification = %ds, want ~5400", s.CertifiedAgainstAge)
	}
	got, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil || got.BasisSHA256 != cert.BasisSHA256 {
		t.Fatalf("GetCertification basis = %q (err %v), want %q", got.BasisSHA256, err, cert.BasisSHA256)
	}
}

// section 11: "unique certification basis hash". Identical evidence is one basis,
// wherever and whenever it is certified: a different run, a different moment.
func TestCertify_TheSameBasisTwice_IsRefused_AndNamesTheExistingOne(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-dup")
	asOf := time.Now().Add(-10 * time.Minute).UTC()

	first := f.runAt(v, "pop-basis", &asOf)
	cert1, err := f.certifyWith(f.s, first.RunID, domain.CertifyDQRequest{})
	if err != nil {
		t.Fatalf("first certify: %v", err)
	}
	// A SECOND, separate run over identical evidence (same ruleset version,
	// population, outcomes, observation time).
	second := f.runAt(v, "pop-basis", &asOf)
	if second.RunID == first.RunID {
		t.Fatal("expected a distinct run")
	}
	_, err = f.certifyWith(f.s, second.RunID, domain.CertifyDQRequest{})
	if !errors.Is(err, domain.ErrCertificationBasisExists) {
		t.Fatalf("err = %v, want ErrCertificationBasisExists", err)
	}
	if !strings.Contains(err.Error(), cert1.CertificationID) {
		t.Fatalf("the refusal should name the existing certification %s: %v", cert1.CertificationID, err)
	}
	if got, _ := f.s.GetRun(f.ctx, orgA, second.RunID); got.Status != domain.RunRunning {
		t.Fatalf("a refused certification must leave the run Running, is %s", got.Status)
	}
}

// ...but a genuinely later observation is a different basis and certifies — so
// uniqueness does not block honest re-certification after evidence is refreshed.
func TestCertify_LaterObservation_IsADifferentBasis(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-later")
	t1 := time.Now().Add(-30 * time.Minute).UTC()
	t2 := t1.Add(10 * time.Minute)

	c1, err := f.certifyWith(f.s, f.runAt(v, "pop-later", &t1).RunID, domain.CertifyDQRequest{})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	c2, err := f.certifyWith(f.s, f.runAt(v, "pop-later", &t2).RunID, domain.CertifyDQRequest{})
	if err != nil {
		t.Fatalf("a later observation of the same population should certify: %v", err)
	}
	if c1.BasisSHA256 == c2.BasisSHA256 {
		t.Fatal("different observation times produced the same basis")
	}
}

// The database enforces it too, for any other writer.
func TestDatabase_RefusesTwoCertificationsOfOneBasis(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-db")
	run1 := f.runAt(v, "pop-db-1", nil)
	cert, err := f.certifyWith(f.s, run1.RunID, domain.CertifyDQRequest{})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	run2 := f.runAt(v, "pop-db-2", nil)
	_, err = f.admin.Exec(f.ctx, `
		INSERT INTO dq_certifications (certification_id, tenant_id, run_id, summary, summary_sha256, certified_by,
			basis_sha256, evidence_as_of, evidence_max_age_seconds)
		VALUES ('dqc_00000000-0000-0000-0000-0000000000f1', $1, $2, '{}'::jsonb, $3, 'raw-writer', $4, now(), 3600)`,
		orgA, run2.RunID, hashLike("s"), cert.BasisSHA256)
	if err == nil {
		t.Fatal("database accepted a second certification with the same basis_sha256")
	}
	// And prove it is the uniqueness that refused it, not a malformed row: the
	// same insert with a different basis is accepted.
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO dq_certifications (certification_id, tenant_id, run_id, summary, summary_sha256, certified_by,
			basis_sha256, evidence_as_of, evidence_max_age_seconds)
		VALUES ('dqc_00000000-0000-0000-0000-0000000000f2', $1, $2, '{}'::jsonb, $3, 'raw-writer', $4, now(), 3600)`,
		orgA, run2.RunID, hashLike("s"), hashLike("a different basis")); err != nil {
		t.Fatalf("database refused a well-formed certification: %v", err)
	}
}

// DQC-05: the lineage manifest a certification rests on is part of its basis.
func TestCertify_LineageManifestReference_IsBoundIntoTheBasis(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-lineage")
	asOf := time.Now().Add(-5 * time.Minute).UTC()
	idA := "dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	idB := "dpm_bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	a, err := f.certifyWith(f.s, f.runAt(v, "pop-lin", &asOf).RunID, lineageRef(idA))
	if err != nil {
		t.Fatalf("certify with manifest A: %v", err)
	}
	if a.LineageManifestID == nil || *a.LineageManifestID != idA || a.Summary.LineageManifestSHA256 == nil {
		t.Fatalf("lineage reference not recorded: %+v", a)
	}
	// Identical evidence over a DIFFERENT lineage is a different basis.
	b, err := f.certifyWith(f.s, f.runAt(v, "pop-lin", &asOf).RunID, lineageRef(idB))
	if err != nil {
		t.Fatalf("identical evidence over manifest B should be its own basis: %v", err)
	}
	if a.BasisSHA256 == b.BasisSHA256 {
		t.Fatal("the lineage reference did not change the basis")
	}
	// Without a reference, it is a third.
	if c, err := f.certifyWith(f.s, f.runAt(v, "pop-lin", &asOf).RunID, domain.CertifyDQRequest{}); err != nil || c.BasisSHA256 == a.BasisSHA256 {
		t.Fatalf("no-reference certification: %v", err)
	}
}

func TestCertify_MalformedLineageReference_IsRefused(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-lineage-bad")
	run := f.runAt(v, "pop-lin-bad", nil)
	id, sha := "dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", goodLineageSHA
	badID, badSHA := "not-a-manifest", "ABC"

	for name, req := range map[string]domain.CertifyDQRequest{
		"id without sha":   {LineageManifestID: &id},
		"sha without id":   {LineageManifestSHA256: &sha},
		"malformed id":     {LineageManifestID: &badID, LineageManifestSHA256: &sha},
		"malformed sha256": {LineageManifestID: &id, LineageManifestSHA256: &badSHA},
	} {
		if _, err := f.certifyWith(f.s, run.RunID, req); err == nil {
			t.Errorf("%s: certification accepted a malformed lineage reference", name)
		}
	}
	// None of the refusals consumed the run.
	if _, err := f.certifyWith(f.s, run.RunID, domain.CertifyDQRequest{}); err != nil {
		t.Fatalf("run should still certify after refused attempts: %v", err)
	}
}

// Certifications sealed before this migration have no basis and must stay
// readable — they are not rewritten or backfilled with a guess.
func TestGetCertification_ACertificationSealedBeforeTheBasisExisted_StillReads(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-legacy")
	run := f.runAt(v, "pop-legacy", nil)
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO dq_certifications (certification_id, tenant_id, run_id, summary, summary_sha256, certified_by)
		VALUES ('dqc_00000000-0000-0000-0000-0000000000e1', $1, $2,
			'{"run_id":"x","ruleset_version_id":"y","population_ref":"z","population_row_count":1,"population_content_hash":"h","results":[]}'::jsonb,
			$3, 'legacy-writer')`, orgA, run.RunID, hashLike("legacy")); err != nil {
		t.Fatalf("seed legacy certification: %v", err)
	}
	got, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatalf("read legacy certification: %v", err)
	}
	if got.BasisSHA256 != "" || got.LineageManifestID != nil {
		t.Fatalf("a legacy certification must not acquire a basis: %+v", got)
	}
}

// A half-populated new-style certification would read as evidenced while
// proving nothing; the database refuses it.
func TestDatabase_RefusesACertificationWithABasisButNoFreshnessState(t *testing.T) {
	f := newFixture(t)
	v := oneRule(f, "basis-partial")
	run := f.runAt(v, "pop-partial", nil)
	_, err := f.admin.Exec(f.ctx, `
		INSERT INTO dq_certifications (certification_id, tenant_id, run_id, summary, summary_sha256, certified_by, basis_sha256)
		VALUES ('dqc_00000000-0000-0000-0000-0000000000d1', $1, $2, '{}'::jsonb, $3, 'raw-writer', $4)`,
		orgA, run.RunID, hashLike("s"), hashLike("b"))
	if err == nil {
		t.Fatal("database accepted a basis with no evidence_as_of / window")
	}
}
