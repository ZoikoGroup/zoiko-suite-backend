package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/data-quality-svc/internal/domain"
)

// The reverse of the blast radius: what does this object rest on, and has any of it been withdrawn?

func subject(service, typ, id string) domain.RelianceSubject {
	return domain.RelianceSubject{RelyingService: service, RelyingObjectType: typ, RelyingObjectID: id}
}

func TestRelianceStanding_ListsEveryCertificationAnObjectRestsOnWithItsStatus(t *testing.T) {
	f := newContractFixture(t)
	active, _ := f.certified("standing-active")
	withdrawn, _ := f.certified("standing-revoked")
	c := f.published(orgA, "standing.window", windowed(3600))
	stale := f.certifiedAgainst(c, "pop-standing-window")
	unrelated, _ := f.certified("standing-unrelated")

	mine := rely("gl-svc", "journal-batch", "jb_1")
	for _, runID := range []string{active.RunID, withdrawn.RunID, stale.RunID} {
		if _, _, err := f.relyOn(runID, mine); err != nil {
			t.Fatal(err)
		}
	}
	// Another object, and another object of the same service, rest on other things: not in this answer.
	if _, _, err := f.relyOn(unrelated.RunID, rely("gl-svc", "journal-batch", "jb_other")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.relyOn(active.RunID, rely("gl-svc", "journal-batch", "jb_other")); err != nil {
		t.Fatal(err)
	}

	before, err := f.s.GetRelianceStanding(f.ctx, orgA, subject("gl-svc", "journal-batch", "jb_1"))
	if err != nil || before.Count != 3 || before.Withdrawn != 0 || before.Stale != 0 {
		t.Fatalf("before any withdrawal: %+v %v", before, err)
	}
	if _, err := f.revoke(withdrawn.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	f.clockAt(stale.ValidUntil.Add(time.Second))

	got, err := f.s.GetRelianceStanding(f.ctx, orgA, subject("gl-svc", "journal-batch", "jb_1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Count != 3 || got.Withdrawn != 1 || got.Stale != 1 || len(got.Certifications) != 3 {
		t.Fatalf("standing: %+v", got)
	}
	byRun := map[string]domain.ReliedCertification{}
	for _, c := range got.Certifications {
		byRun[c.Reliance.RunID] = c
	}
	if byRun[active.RunID].Status != domain.CertificationActive || byRun[active.RunID].Revocation != nil {
		t.Fatalf("active: %+v", byRun[active.RunID])
	}
	if r := byRun[withdrawn.RunID]; r.Status != domain.CertificationRevoked || r.Revocation == nil || r.Revocation.ReasonCode != domain.RevokeFalsePass {
		t.Fatalf("revoked must carry its revocation: %+v", r)
	}
	if byRun[stale.RunID].Status != domain.CertificationStale {
		t.Fatalf("stale: %+v", byRun[stale.RunID])
	}
	if _, ok := byRun[unrelated.RunID]; ok {
		t.Fatal("a certification this object never relied on is in its answer")
	}
}

func TestRelianceStanding_NothingRecordedIsAnEmptyListNotAnError(t *testing.T) {
	f := newFixture(t)
	got, err := f.s.GetRelianceStanding(f.ctx, orgA, subject("gl-svc", "journal-batch", "never-recorded"))
	if err != nil || got.Count != 0 || got.Certifications == nil {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestRelianceStanding_IsTenantIsolatedAndValidated(t *testing.T) {
	f := newFixture(t)
	run, _ := f.certified("standing-tenant")
	if _, _, err := f.relyOn(run.RunID, rely("gl-svc", "journal-batch", "jb_1")); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.GetRelianceStanding(f.ctx, orgB, subject("gl-svc", "journal-batch", "jb_1")); err != nil || got.Count != 0 {
		t.Fatalf("another tenant saw %+v (%v)", got, err)
	}
	for name, s := range map[string]domain.RelianceSubject{
		"no service":   subject("", "journal-batch", "jb_1"),
		"bad type":     subject("gl-svc", "journal batch", "jb_1"),
		"injection id": subject("gl-svc", "journal-batch", "x' OR '1'='1"),
		"too long id":  subject("gl-svc", "journal-batch", string(make([]byte, 129))),
	} {
		if _, err := f.s.GetRelianceStanding(f.ctx, orgA, s); !errors.Is(err, domain.ErrRelianceInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
