package store_test

import (
	"errors"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// The reverse of the blast radius: what does this object rest on, and has any of it been withdrawn or replaced?

func subject(service, typ, id string) domain.RelianceSubject {
	return domain.RelianceSubject{RelyingService: service, RelyingObjectType: typ, RelyingObjectID: id}
}

func TestRelianceStanding_ListsEveryCertificationAnObjectRestsOnWithItsState(t *testing.T) {
	f := newFixture(t)
	inForce := f.certifiedRun("standing-in-force")
	revoked := f.certifiedRun("standing-revoked")
	unrelated := f.certifiedRun("standing-unrelated")

	// A run that gets superseded by a corrected one.
	def := f.defineReconciliation(orgA, "standing-superseded", 0)
	oldRun := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(oldRun.RunID)

	mine := rely("gl-svc", "journal-batch", "jb_1")
	for _, runID := range []string{inForce.RunID, revoked.RunID, oldRun.RunID} {
		if _, _, err := f.relyOn(runID, mine); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.relyOn(unrelated.RunID, rely("gl-svc", "journal-batch", "jb_other")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(inForce.RunID, rely("gl-svc", "journal-batch", "jb_other")); err != nil {
		t.Fatal(err)
	}

	before, err := f.s.GetRelianceStanding(f.ctx, orgA, entityA, subject("gl-svc", "journal-batch", "jb_1"))
	if err != nil || before.Count != 3 || before.Withdrawn != 0 || before.Superseded != 0 {
		t.Fatalf("before any change: %+v %v", before, err)
	}

	if _, err := f.revokeRun(revoked.RunID); err != nil {
		t.Fatal(err)
	}
	newRun := f.startRun(orgA, def, cleanPops())
	f.matchAndCertify(newRun.RunID)
	if _, err := f.s.SupersedeRun(f.ctx, orgA, oldRun.RunID, domain.SupersedeRunRequest{NewRunID: newRun.RunID, Reason: "corrected"}, "controller-carol",
		f.claim("Supersede", oldRun.RunID)); err != nil {
		t.Fatal(err)
	}

	got, err := f.s.GetRelianceStanding(f.ctx, orgA, entityA, subject("gl-svc", "journal-batch", "jb_1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.LegalEntityID != entityA || got.Count != 3 || got.Withdrawn != 1 || got.Superseded != 1 {
		t.Fatalf("standing: %+v", got)
	}
	byRun := map[string]domain.ReliedCertification{}
	for _, c := range got.Certifications {
		byRun[c.Reliance.RunID] = c
	}
	if c := byRun[inForce.RunID]; c.CertificationState != domain.CertCertified || c.Revocation != nil {
		t.Fatalf("in force: %+v", c)
	}
	if c := byRun[revoked.RunID]; c.CertificationState != domain.CertRevoked || c.Revocation == nil || !strings.Contains(c.Revocation.Reason, "restated") {
		t.Fatalf("revoked must carry its revocation: %+v", c)
	}
	if c := byRun[oldRun.RunID]; c.CertificationState != domain.CertSuperseded {
		t.Fatalf("superseded: %+v", c)
	}
	if _, ok := byRun[unrelated.RunID]; ok {
		t.Fatal("a certification this object never relied on is in its answer")
	}
}

func TestRelianceStanding_NeverSpansLegalEntities(t *testing.T) {
	f := newFixture(t)
	run := f.certifiedRun("standing-entity")
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetRelianceStanding(f.ctx, orgA, entityB, subject("gl-svc", "journal-batch", "jb_1")); err != nil || got.Count != 0 {
		t.Fatalf("another entity's answer leaked this entity's certification: %+v (%v)", got, err)
	}
	if got, err := f.s.GetRelianceStanding(f.ctx, orgA, entityA, subject("gl-svc", "journal-batch", "jb_1")); err != nil || got.Count != 1 {
		t.Fatalf("the owning entity: %+v (%v)", got, err)
	}
}

func TestRelianceStanding_NothingRecordedIsEmptyAndTenantsAreIsolatedAndInputIsValidated(t *testing.T) {
	f := newFixture(t)
	if got, err := f.s.GetRelianceStanding(f.ctx, orgA, entityA, subject("gl-svc", "journal-batch", "never")); err != nil || got.Count != 0 || got.Certifications == nil {
		t.Fatalf("nothing recorded: %+v %v", got, err)
	}
	run := f.certifiedRun("standing-tenant")
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetRelianceStanding(f.ctx, orgB, entityA, subject("gl-svc", "journal-batch", "jb_1")); err != nil || got.Count != 0 {
		t.Fatalf("another tenant saw %+v (%v)", got, err)
	}
	for name, s := range map[string]domain.RelianceSubject{
		"no service":   subject("", "journal-batch", "jb_1"),
		"bad type":     subject("gl-svc", "journal batch", "jb_1"),
		"injection id": subject("gl-svc", "journal-batch", "x' OR '1'='1"),
		"too long id":  subject("gl-svc", "journal-batch", strings.Repeat("a", 129)),
	} {
		if _, err := f.s.GetRelianceStanding(f.ctx, orgA, entityA, s); !errors.Is(err, domain.ErrRelianceInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
