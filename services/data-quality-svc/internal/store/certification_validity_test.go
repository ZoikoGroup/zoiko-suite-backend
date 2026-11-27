package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/data-quality-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-016 CERTIFICATION_STALE (docs/architecture/DQC_GAP_MATRIX.md, step H): a contract can state how long
// a certification stays reliable; past it the certification reads as STALE.

func windowed(seconds int64) domain.ContractBody {
	b := profiled(domain.CompatBackward)
	b.CertificationProfile.ValidityWindowSeconds = seconds
	return b
}

// clockAt lets a test read a certification at a chosen instant, after it was issued.
func (f *fixture) clockAt(t time.Time) { f.s.WithClock(func() time.Time { return t }) }

func (f *fixture) certifiedAgainst(c *domain.ContractVersion, ref string) *domain.DQCertification {
	f.t.Helper()
	f.clockAt(time.Now().UTC())
	run := f.mustContractRun(c, ref, allPass()...)
	cert, err := f.certify(run)
	if err != nil {
		f.t.Fatalf("certify: %v", err)
	}
	return cert
}

func (f *fixture) statusAt(runID string, at time.Time) *domain.DQCertification {
	f.t.Helper()
	f.clockAt(at)
	got, err := f.s.GetCertification(f.ctx, orgA, runID)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestValidity_CertificationGoesStalePastItsWindow(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "valid.window", windowed(3600))
	cert := f.certifiedAgainst(c, "pop-valid")
	if cert.ValidityWindowSeconds == nil || *cert.ValidityWindowSeconds != 3600 || cert.ValidUntil == nil || cert.Status != domain.CertificationActive {
		t.Fatalf("a new certification: %+v", cert)
	}
	if got := cert.ValidUntil.Sub(cert.CertifiedAt); got != time.Hour {
		t.Fatalf("valid_until is %s after certification, want 1h", got)
	}
	issued := cert.CertifiedAt
	for name, tc := range map[string]struct {
		at   time.Time
		want string
	}{
		"just issued":         {issued.Add(time.Second), domain.CertificationActive},
		"a second before":     {cert.ValidUntil.Add(-time.Second), domain.CertificationActive},
		"exactly at the edge": {*cert.ValidUntil, domain.CertificationActive},
		"a second after":      {cert.ValidUntil.Add(time.Second), domain.CertificationStale},
		"a year later":        {issued.Add(365 * 24 * time.Hour), domain.CertificationStale},
	} {
		if got := f.statusAt(cert.RunID, tc.at); got.Status != tc.want || got.ValidUntil == nil || !got.ValidUntil.Equal(*cert.ValidUntil) {
			t.Fatalf("%s: status %s (valid_until %v), want %s", name, got.Status, got.ValidUntil, tc.want)
		}
	}
}

func TestValidity_NoWindowNeverGoesStale(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "valid.none", windowed(0))
	cert := f.certifiedAgainst(c, "pop-none")
	if cert.ValidUntil != nil || cert.ValidityWindowSeconds != nil {
		t.Fatalf("a contract with no window gave the certification one: %+v", cert)
	}
	if got := f.statusAt(cert.RunID, cert.CertifiedAt.Add(10*365*24*time.Hour)); got.Status != domain.CertificationActive || got.ValidUntil != nil {
		t.Fatalf("ten years later: %+v", got)
	}
	// A rule-set certification has no contract and so no window.
	f.clockAt(time.Now().UTC())
	_, legacy := f.certified("valid-legacy")
	if legacy.ValidUntil != nil {
		t.Fatalf("a rule-set certification has a window: %+v", legacy)
	}
}

// Revocation is a deliberate act with a reason; it outranks the passage of time.
func TestValidity_RevokedOutranksStale(t *testing.T) {
	f := newContractFixture(t)
	c := f.published(orgA, "valid.revoked", windowed(60))
	cert := f.certifiedAgainst(c, "pop-revoked")
	if _, err := f.revoke(cert.RunID, "control-owner", falsePass); err != nil {
		t.Fatal(err)
	}
	if got := f.statusAt(cert.RunID, cert.ValidUntil.Add(time.Hour)); got.Status != domain.CertificationRevoked || got.Revocation == nil {
		t.Fatalf("a revoked certification past its window: %+v", got)
	}
}

// The window a certification was issued under is frozen on it: a later contract version with a different window
// does not move it.
func TestValidity_WindowIsFrozenAtIssue(t *testing.T) {
	f := newContractFixture(t)
	c1 := f.published(orgA, "valid.frozen", windowed(3600))
	cert := f.certifiedAgainst(c1, "pop-frozen")
	b2 := windowed(60)
	b2.Schema = append(b2.Schema, domain.SchemaField{Name: "note", Type: "string", Nullable: true})
	v2 := f.mustCreate(orgA, "valid.frozen", b2)
	if _, err := f.submit(v2, reviewer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.publish(v2, publisher, false); err != nil {
		t.Fatal(err)
	}
	got := f.statusAt(cert.RunID, cert.CertifiedAt.Add(30*time.Minute))
	if got.Status != domain.CertificationActive || *got.ValidityWindowSeconds != 3600 {
		t.Fatalf("the certification took the later version's 60s window: %+v", got)
	}
}

func TestValidity_ContractRefusesAnImpossibleWindow(t *testing.T) {
	f := newContractFixture(t)
	for name, w := range map[string]int64{"negative": -1, "over ten years": domain.MaxValidityWindowSeconds + 1} {
		if _, err := f.create(orgA, "valid.bad."+map[string]string{"negative": "neg", "over ten years": "big"}[name], windowed(w)); !errors.Is(err, domain.ErrContractInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// The boundaries themselves are accepted: it was the out-of-range value that was refused.
	for name, w := range map[string]int64{"zero": 0, "one second": 1, "ten years": domain.MaxValidityWindowSeconds} {
		if _, err := f.create(orgA, "valid.ok."+map[string]string{"zero": "zero", "one second": "one", "ten years": "max"}[name], windowed(w)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestValidity_DatabaseKeepsWindowAndValidUntilTogetherAndAfterIssue(t *testing.T) {
	f := newContractFixture(t)
	f.clockAt(time.Now().UTC())
	_, cert := f.certified("valid-db")
	// The sealed certification cannot be edited to move its window.
	if _, err := f.admin.Exec(f.ctx, `UPDATE dq_certifications SET valid_until = now() + interval '100 years' WHERE certification_id = $1`, cert.CertificationID); err == nil {
		t.Fatal("the database allowed a sealed certification's validity to be changed")
	}
	// The CHECKs are what is being proved, so the immutability trigger (which would refuse these edits first) is
	// relaxed for the rest of the test.
	_, c2 := f.certified("valid-db-2")
	if _, err := f.admin.Exec(f.ctx, `ALTER TABLE dq_certifications DISABLE TRIGGER trigger_dq_certification_immutability`); err != nil {
		t.Skipf("cannot relax the immutability trigger for this check: %v", err)
	}
	defer f.admin.Exec(f.ctx, `ALTER TABLE dq_certifications ENABLE TRIGGER trigger_dq_certification_immutability`) //nolint:errcheck
	for name, sql := range map[string]string{
		"window without valid_until": `UPDATE dq_certifications SET validity_window_seconds = 60 WHERE certification_id = $1`,
		"valid_until without window": `UPDATE dq_certifications SET valid_until = certified_at + interval '1 hour' WHERE certification_id = $1`,
		"zero window":                `UPDATE dq_certifications SET validity_window_seconds = 0, valid_until = certified_at + interval '1 hour' WHERE certification_id = $1`,
		"valid_until before issue":   `UPDATE dq_certifications SET validity_window_seconds = 60, valid_until = certified_at - interval '1 minute' WHERE certification_id = $1`,
	} {
		if _, err := f.admin.Exec(f.ctx, sql, c2.CertificationID); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := f.admin.Exec(f.ctx, `UPDATE dq_certifications SET validity_window_seconds = 60, valid_until = certified_at + interval '60 seconds' WHERE certification_id = $1`, c2.CertificationID); err != nil {
		t.Fatalf("a consistent window was refused: %v", err)
	}
}
