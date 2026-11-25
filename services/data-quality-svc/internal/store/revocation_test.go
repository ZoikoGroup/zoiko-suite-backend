package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, step F3): a certification can be
// revoked, prospectively, without touching the sealed evidence.

func (f *fixture) certified(name string) (*domain.DQRun, *domain.DQCertification) {
	f.t.Helper()
	v := f.publishRuleSet(orgA, name, []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	run, err := f.run(v, "pop-"+name, passingOutcome("r-a"))
	if err != nil {
		f.t.Fatal(err)
	}
	cert, err := f.certify(run)
	if err != nil {
		f.t.Fatal(err)
	}
	return run, cert
}

func (f *fixture) revoke(runID, actor string, req domain.RevokeCertificationRequest) (*domain.CertificationRevocation, error) {
	return f.s.RevokeCertification(f.ctx, orgA, runID, actor, req, f.claim("RevokeCertification", runID))
}

var falsePass = domain.RevokeCertificationRequest{ReasonCode: domain.RevokeFalsePass, Reason: "control defect: rule r-a never read the amended rows",
	EvidenceRefs: []string{"incident-4471"}}

func TestRevocation_WithdrawsRelianceAndPreservesTheSealedEvidence(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("rev-basic")
	if cert.Status != domain.CertificationActive {
		t.Fatalf("a new certification is %q, want ACTIVE", cert.Status)
	}
	var summaryBefore string
	if err := f.admin.QueryRow(f.ctx, `SELECT summary::text FROM dq_certifications WHERE certification_id=$1`, cert.CertificationID).Scan(&summaryBefore); err != nil {
		t.Fatal(err)
	}

	before := time.Now().UTC().Add(-time.Second)
	rev, err := f.revoke(run.RunID, "control-owner", falsePass)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if rev.CertificationID != cert.CertificationID || rev.RevokedBy != "control-owner" || rev.ReasonCode != domain.RevokeFalsePass || rev.RevokedAt.Before(before) {
		t.Fatalf("revocation: %+v", rev)
	}

	got, err := f.s.GetCertification(f.ctx, orgA, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.CertificationRevoked || got.Revocation == nil || got.Revocation.RevocationID != rev.RevocationID {
		t.Fatalf("the certification does not read as revoked: %+v", got)
	}
	// Preserve prior evidence: the sealed certification is byte for byte what it was, and so is its hash.
	var summaryAfter string
	if err := f.admin.QueryRow(f.ctx, `SELECT summary::text FROM dq_certifications WHERE certification_id=$1`, cert.CertificationID).Scan(&summaryAfter); err != nil {
		t.Fatal(err)
	}
	if summaryAfter != summaryBefore || got.SummarySHA256 != cert.SummarySHA256 || got.BasisSHA256 != cert.BasisSHA256 {
		t.Fatal("revocation changed the sealed certification")
	}
	// The run keeps its Certified status: revocation is not a rewrite of history.
	if r, _ := f.s.GetRun(f.ctx, orgA, run.RunID); r.Status != domain.RunCertified {
		t.Fatalf("revocation changed the run to %s", r.Status)
	}
}

func TestRevocation_OnlyOnce_AndOnlyOfARealCertification(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("rev-once")
	if _, err := f.revoke(run.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke(run.RunID, "someone-else", falsePass); !errors.Is(err, domain.ErrCertificationAlreadyRevoked) {
		t.Fatalf("a second revocation: %v", err)
	}
	// A run that was never certified has nothing to revoke.
	v := f.publishRuleSet(orgA, "rev-uncertified", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	open, err := f.run(v, "pop-rev-uncertified", passingOutcome("r-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke(open.RunID, "control-owner", falsePass); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("revoking an uncertified run: %v", err)
	}
	if _, err := f.revoke("dqn_00000000-0000-0000-0000-000000000000", "control-owner", falsePass); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("revoking an unknown run: %v", err)
	}
}

func TestRevocation_RequiresAKnownReasonCodeAndAReason(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("rev-reason")
	for name, req := range map[string]domain.RevokeCertificationRequest{
		"no code":      {Reason: "because"},
		"unknown code": {ReasonCode: "BORED", Reason: "because"},
		"no reason":    {ReasonCode: domain.RevokeFalsePass},
		"blank reason": {ReasonCode: domain.RevokeFalsePass, Reason: "   "},
	} {
		if _, err := f.revoke(run.RunID, "control-owner", req); !errors.Is(err, domain.ErrRevocationInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Proof that those were refused for their reason: a valid request for the same run is accepted.
	if _, err := f.revoke(run.RunID, "control-owner", domain.RevokeCertificationRequest{ReasonCode: domain.RevokeOther, Reason: "x", EvidenceRefs: []string{"e"}}); err != nil {
		t.Fatalf("a valid revocation was refused: %v", err)
	}
}

// The basis stays claimed: revoking a false pass does not free the same evidence to be certified again.
func TestRevocation_TheSameEvidenceCannotBeCertifiedAgain(t *testing.T) {
	f := newFixture(t)
	v := f.publishRuleSet(orgA, "rev-basis", []domain.RuleDefinition{{RuleKey: "r-a", Description: "x"}})
	observed := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	mk := func() *domain.DQRun {
		r, err := f.s.RunDQ(f.ctx, orgA, domain.RunDQRequest{RuleSetVersionID: v.VersionID, PopulationRef: "pop-rev-basis", PopulationRowCount: 100,
			PopulationContentHash: hashLike("pop-rev-basis"), RuleOutcomes: []domain.RuleOutcome{passingOutcome("r-a")}, EvidenceAsOf: &observed}, "test-operator", f.claim("RunDQ", "rev-basis"))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	first := mk()
	if _, err := f.certify(first); err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke(first.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	if _, err := f.certify(mk()); !errors.Is(err, domain.ErrCertificationBasisExists) {
		t.Fatalf("re-certifying revoked evidence: %v", err)
	}
}

func TestRevocation_IsTenantScoped(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("rev-tenant")
	if _, err := f.s.RevokeCertification(f.ctx, orgB, run.RunID, "intruder", falsePass, f.claim("RevokeCertification", "x")); !errors.Is(err, domain.ErrCertificationNotFound) {
		t.Fatalf("tenant B revoked tenant A's certification: %v", err)
	}
	if got, _ := f.s.GetCertification(f.ctx, orgA, run.RunID); got.Status != domain.CertificationActive {
		t.Fatalf("a refused cross-tenant revocation changed the status: %s", got.Status)
	}
}

func TestRevocation_IdempotentReplayAndReuse(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("rev-idem")
	claim := f.claim("RevokeCertification", run.RunID)
	first, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, "control-owner", falsePass, claim)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.RevokeCertification(f.ctx, orgA, run.RunID, "control-owner", falsePass, claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.RevocationID {
		t.Fatalf("a replay: %v", err)
	}
	claim.RequestSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, "control-owner", falsePass, claim); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
		t.Fatalf("the key reused for a different request: %v", err)
	}
}

// The same guarantees, held by the database.
func TestRevocation_DatabaseKeepsItAppendOnly(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("rev-db")
	rev, err := f.revoke(run.RunID, "control-owner", falsePass)
	if err != nil {
		t.Fatal(err)
	}
	for name, stmt := range map[string]string{
		"update the reason":  `UPDATE dq_certification_revocations SET reason = 'changed' WHERE revocation_id = $1`,
		"delete (un-revoke)": `DELETE FROM dq_certification_revocations WHERE revocation_id = $1`,
	} {
		if _, err := f.admin.Exec(f.ctx, stmt, rev.RevocationID); err == nil {
			t.Fatalf("the database allowed: %s", name)
		}
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE dq_certifications SET certified_by = 'x' WHERE certification_id = $1`, cert.CertificationID); err == nil {
		t.Fatal("the database allowed the sealed certification to be edited")
	}
	insert := `INSERT INTO dq_certification_revocations (revocation_id, tenant_id, certification_id, reason_code, reason, evidence_refs, certified_by, certified_at, revoked_by)
		VALUES ($1, $2, $3, $4, $5, ARRAY['e'], 'test-operator', now() - interval '1 hour', 'x')`
	if _, err := f.admin.Exec(f.ctx, insert, newTestRevocationID(), orgA, cert.CertificationID, "FALSE_PASS", "again"); err == nil {
		t.Fatal("a second revocation of one certification was accepted")
	}
	// Proof that the refusals above are about those rules: a fresh certification takes a valid revocation,
	// and an unknown reason code is refused.
	_, c2 := f.certified("rev-db-2")
	if _, err := f.admin.Exec(f.ctx, insert, newTestRevocationID(), orgA, c2.CertificationID, "BORED", "x"); err == nil {
		t.Fatal("an unknown reason code was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRevocationID(), orgA, c2.CertificationID, "FALSE_PASS", "   "); err == nil {
		t.Fatal("a blank reason was accepted")
	}
	if _, err := f.admin.Exec(f.ctx, insert, newTestRevocationID(), orgA, c2.CertificationID, "FALSE_PASS", "valid"); err != nil {
		t.Fatalf("a valid revocation was refused: %v", err)
	}
}

func TestRevocation_EventsIssuedAndRevoked(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("rev-events")
	count := func(event string) int {
		var n int
		_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type=$2`, cert.CertificationID, event).Scan(&n)
		return n
	}
	if count("dqc.certification.issued") != 1 || count("DATA.DQCertified") != 1 || count("dqc.certification.revoked") != 0 {
		t.Fatalf("after certifying: issued=%d legacy=%d revoked=%d", count("dqc.certification.issued"), count("DATA.DQCertified"), count("dqc.certification.revoked"))
	}
	if _, err := f.revoke(run.RunID, "control-owner", domain.RevokeCertificationRequest{ReasonCode: "BORED", Reason: "x", EvidenceRefs: []string{"e"}}); err == nil {
		t.Fatal("setup")
	}
	if count("dqc.certification.revoked") != 0 {
		t.Fatal("a refused revocation wrote an event")
	}
	if _, err := f.revoke(run.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	if _, err := f.revoke(run.RunID, "control-owner", falsePass); err == nil {
		t.Fatal("setup")
	}
	if count("dqc.certification.revoked") != 1 {
		t.Fatalf("revoked events = %d, want exactly 1", count("dqc.certification.revoked"))
	}
	var reason string
	if err := f.admin.QueryRow(f.ctx, `SELECT payload->>'reason_code' FROM outbox_events WHERE aggregate_id=$1 AND event_type='dqc.certification.revoked'`, cert.CertificationID).Scan(&reason); err != nil || reason != "FALSE_PASS" {
		t.Fatalf("event payload reason_code = %q (%v)", reason, err)
	}
}

func newTestRevocationID() string { return "dqx_" + uuid.NewString() }

// The certifier cannot revoke their own certification (as in reconciliation-engine-svc), and a refused
// attempt does not consume the idempotency key.
func TestRevocation_CertifierCannotRevoke(t *testing.T) {
	f := newFixture(t)
	run, cert := f.certified("rev-independence") // certified by "test-operator"
	claim := f.claim("RevokeCertification", run.RunID)
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, "test-operator", falsePass, claim); !errors.Is(err, domain.ErrRevokerIsCertifier) {
		t.Fatalf("the certifier revoked their own certification: %v", err)
	}
	var keys int
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*) FROM idempotency_keys WHERE idempotency_key = $1`, claim.Key).Scan(&keys); err != nil || keys != 0 {
		t.Fatalf("a refused revocation consumed its key: %d, %v", keys, err)
	}
	if got, _ := f.s.GetCertification(f.ctx, orgA, run.RunID); got.Status != domain.CertificationActive || got.CertificationID != cert.CertificationID {
		t.Fatalf("a refused revocation changed the certification: %+v", got)
	}
	// Someone else can: it was independence, not the request, that was refused.
	if _, err := f.s.RevokeCertification(f.ctx, orgA, run.RunID, "someone-independent", falsePass, claim); err != nil {
		t.Fatalf("an independent revoker was refused: %v", err)
	}
}

// "Certification revoked with no reason/evidence": evidence is mandatory, and bounded.
func TestRevocation_RequiresEvidence(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("rev-evidence")
	tooMany := make([]string, 21)
	for i := range tooMany {
		tooMany[i] = "e"
	}
	for name, refs := range map[string][]string{
		"none": nil, "empty list": {}, "blank ref": {"ok", "  "}, "too long": {string(make([]byte, 501))}, "too many": tooMany,
	} {
		req := falsePass
		req.EvidenceRefs = refs
		if _, err := f.revoke(run.RunID, "control-owner", req); !errors.Is(err, domain.ErrRevocationInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got, _ := f.s.GetCertification(f.ctx, orgA, run.RunID); got.Status != domain.CertificationActive {
		t.Fatal("a refused revocation changed the status")
	}
	rev, err := f.revoke(run.RunID, "control-owner", falsePass)
	if err != nil || len(rev.EvidenceRefs) != 1 || rev.EvidenceRefs[0] != "incident-4471" || rev.CertifiedBy != "test-operator" {
		t.Fatalf("a revocation with evidence: %+v %v", rev, err)
	}
}

func TestRevocation_DatabaseRequiresEvidenceIndependenceAndProspectiveTime(t *testing.T) {
	f := newFixture(t)
	_, cert := f.certified("rev-db-checks")
	insert := `INSERT INTO dq_certification_revocations (revocation_id, tenant_id, certification_id, reason_code, reason, evidence_refs, certified_by, certified_at, revoked_by, revoked_at)
		VALUES ($1, $2, $3, 'FALSE_PASS', 'r', $4, $5, $6, $7, $8)`
	certAt := time.Now().UTC().Add(-time.Hour)
	try := func(refs []string, certBy, revBy string, revAt time.Time) error {
		_, err := f.admin.Exec(f.ctx, insert, newTestRevocationID(), orgA, cert.CertificationID, refs, certBy, certAt, revBy, revAt)
		return err
	}
	now := time.Now().UTC()
	if try(nil, "a", "b", now) == nil || try([]string{}, "a", "b", now) == nil {
		t.Fatal("a revocation with no evidence was accepted")
	}
	if try([]string{"ok", " "}, "a", "b", now) == nil {
		t.Fatal("a revocation with a blank evidence reference was accepted")
	}
	if try([]string{"e"}, "same", "same", now) == nil {
		t.Fatal("a revocation by the certifier was accepted")
	}
	if try([]string{"e"}, "a", "b", certAt.Add(-time.Minute)) == nil {
		t.Fatal("a revocation dated before the certification was accepted")
	}
	// Proof that each refusal was about its own rule: the same row with all three satisfied is accepted.
	if err := try([]string{"e"}, "a", "b", now); err != nil {
		t.Fatalf("a valid revocation was refused: %v", err)
	}
}
