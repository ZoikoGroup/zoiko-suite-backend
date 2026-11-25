package store_test

import (
	"fmt"
	"strings"
	"testing"

	"zoiko.io/reconciliation-engine-svc/internal/domain"
)

// ZS-SVC-Z-001 INV-24 / INV-15 / NP-60 (docs/architecture/DQC_GAP_MATRIX.md, step D).
//
// A content hash is evidence only if it can be re-derived, and it can only be
// re-derived if it says how it was made.

func (f *fixture) verified(runID string) []domain.PopulationHashCheck {
	f.t.Helper()
	checks, err := f.s.VerifyPopulationHashes(f.ctx, orgA, runID)
	if err != nil {
		f.t.Fatalf("verify population hashes: %v", err)
	}
	return checks
}

// pauseTrigger lets a test simulate corruption of rows the database otherwise
// refuses to change (what a bad restore or a rogue writer would look like), and
// puts the trigger back when the test ends.
func (f *fixture) pauseTrigger(table, trigger string) {
	f.t.Helper()
	if _, err := f.admin.Exec(f.ctx, `ALTER TABLE `+table+` DISABLE TRIGGER `+trigger); err != nil {
		f.t.Fatalf("pause %s: %v", trigger, err)
	}
	f.t.Cleanup(func() { _, _ = f.admin.Exec(f.ctx, `ALTER TABLE `+table+` ENABLE TRIGGER `+trigger) })
}

func TestSnapshots_RecordTheFormatTheirHashWasMadeUnder(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "hash-format-record", 0)
	run := f.startRun(orgA, def, cleanPops())
	f.match(run.RunID)
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(cleanPops()), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	snaps, err := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	if err != nil || len(snaps) != 4 {
		t.Fatalf("snapshots: %d (err %v), want 2 sides x 2 generations", len(snaps), err)
	}
	for _, s := range snaps {
		if s.HashAlgorithm != domain.PopulationHashAlgorithm || s.HashSerialization != domain.PopulationHashSerialization {
			t.Fatalf("snapshot %s (gen %d) records %q/%q", s.SnapshotID, s.Generation, s.HashAlgorithm, s.HashSerialization)
		}
	}
}

// Every snapshot, every generation, re-derives to the hash it was frozen with.
func TestVerifyPopulationHashes_AFreshRunVerifiesInEveryGeneration(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "hash-format-verify", 0)
	run := f.startRun(orgA, def, cleanPops())
	f.match(run.RunID)
	if _, err := f.s.Reperform(f.ctx, orgA, run.RunID, frozenAll(cleanPops()), "reperformer", f.claim("Reperform", run.RunID)); err != nil {
		t.Fatalf("reperform: %v", err)
	}
	checks := f.verified(run.RunID)
	if len(checks) != 4 {
		t.Fatalf("checks = %d, want 4", len(checks))
	}
	for _, c := range checks {
		if !c.Verified || c.RecomputedHash != c.StoredHash || c.Reason != "" {
			t.Fatalf("snapshot %s (gen %d) did not verify: %+v", c.SnapshotID, c.Generation, c)
		}
	}
}

// NP-60: an item changed after it was frozen — a bad restore, a rogue writer — is
// caught, and only for the snapshot it belongs to.
func TestVerifyPopulationHashes_ACorruptedItemIsCaught(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "hash-format-tamper", 0)
	run := f.startRun(orgA, def, cleanPops())
	snaps, _ := f.s.GetPopulationSnapshots(f.ctx, orgA, run.RunID)
	var victim string
	for _, s := range snaps {
		if s.Side == "Bank" {
			victim = s.SnapshotID
		}
	}
	const corrupt = `UPDATE population_items SET amount_minor_units = amount_minor_units + 1 WHERE snapshot_id = $1`
	// The database refuses this on its own...
	if _, err := f.admin.Exec(f.ctx, corrupt, victim); err == nil {
		t.Fatal("the database let a frozen item be edited")
	}
	// ...so simulate the corruption that gets around it, and check the verifier sees it.
	f.pauseTrigger("population_items", "trigger_population_items_immutable")
	if _, err := f.admin.Exec(f.ctx, corrupt, victim); err != nil {
		t.Fatalf("simulate corruption: %v", err)
	}
	for _, c := range f.verified(run.RunID) {
		if c.SnapshotID == victim {
			if c.Verified || c.RecomputedHash == c.StoredHash || !strings.Contains(c.Reason, "no longer hash") {
				t.Fatalf("a corrupted snapshot verified: %+v", c)
			}
		} else if !c.Verified {
			t.Fatalf("an untouched snapshot failed verification: %+v", c)
		}
	}
}

// INV-15: a snapshot made under a format this build does not know is reported as
// NOT verified, with the reason. It is never assumed to match, and never
// re-derived under a format it was not made with.
func TestVerifyPopulationHashes_AnUnknownFormatIsReportedNotGuessed(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "hash-format-unknown", 0)
	run := f.startRun(orgA, def, cleanPops())
	f.pauseTrigger("population_snapshots", "trigger_population_snapshots_immutable")
	if _, err := f.admin.Exec(f.ctx, `UPDATE population_snapshots SET hash_serialization = 'rpop-v9' WHERE run_id = $1 AND side = 'Bank'`, run.RunID); err != nil {
		t.Fatalf("set a future serialization: %v", err)
	}
	var unknown, ok int
	for _, c := range f.verified(run.RunID) {
		if c.HashSerialization == "rpop-v9" {
			unknown++
			if c.Verified || c.RecomputedHash != "" || !strings.Contains(c.Reason, "unknown population hash format") {
				t.Fatalf("an unknown format was not reported as such: %+v", c)
			}
		} else if c.Verified {
			ok++
		}
	}
	if unknown != 1 || ok != 1 {
		t.Fatalf("unknown=%d verified=%d, want 1 and 1", unknown, ok)
	}
}

// The database accepts only formats this build can verify, and a writer must say
// which one it used: there is no default to quietly claim the old one.
func TestDatabase_HashFormatIsConstrainedAndMustBeStated(t *testing.T) {
	f := newFixture(t)
	def := f.defineReconciliation(orgA, "hash-format-db", 0)
	run := f.startRun(orgA, def, cleanPops())
	f.pauseTrigger("population_snapshots", "trigger_population_snapshots_immutable")

	set := func(col, val string) error {
		_, err := f.admin.Exec(f.ctx, `UPDATE population_snapshots SET `+col+` = $2 WHERE run_id = $1 AND side = 'Bank'`, run.RunID, val)
		return err
	}
	if err := set("hash_algorithm", "md5"); err == nil {
		t.Fatal("database accepted an algorithm this build cannot verify")
	}
	if err := set("hash_serialization", "whatever"); err == nil {
		t.Fatal("database accepted a malformed serialization identifier")
	}
	// Prove it was the constraints that refused: well-formed values are accepted.
	if err := set("hash_algorithm", "sha256"); err != nil {
		t.Fatalf("database refused a valid algorithm: %v", err)
	}
	if err := set("hash_serialization", "rpop-v2"); err != nil {
		t.Fatalf("database refused a well-formed serialization: %v", err)
	}

	// No default: an insert that does not state its format is refused. Both inserts
	// copy a real snapshot, so the format columns are the only difference.
	const base = `
		INSERT INTO population_snapshots (snapshot_id, tenant_id, run_id, side, generation, source_system, spec_ref, source_watermark,
			item_count, amount_scale, control_totals, declared_row_count, declared_totals, content_hash%s)
		SELECT $2, tenant_id, run_id, 'Extra', 9, source_system, spec_ref, source_watermark,
			item_count, amount_scale, control_totals, declared_row_count, declared_totals, content_hash%s
		FROM population_snapshots WHERE run_id = $1 AND side = 'Bank' LIMIT 1`
	_, err := f.admin.Exec(f.ctx, fmt.Sprintf(base, "", ""), run.RunID, "rps_00000000-0000-0000-0000-0000000000a1")
	if err == nil || !strings.Contains(err.Error(), "hash_algorithm") {
		t.Fatalf("an insert that omits the hash format was accepted: %v", err)
	}
	stating := fmt.Sprintf(base, ", hash_algorithm, hash_serialization", ", 'sha256', 'rpop-v1'")
	if _, err := f.admin.Exec(f.ctx, stating, run.RunID, "rps_00000000-0000-0000-0000-0000000000a2"); err != nil {
		t.Fatalf("database refused a snapshot that states its format: %v", err)
	}
}

// A certification names the formats it rests on, so its evidence can be re-derived.
func TestCertification_NamesTheHashFormatsItRestsOn(t *testing.T) {
	f := newFixture(t)
	def := f.defineWithWindow("hash-format-cert", nil)
	cert, err := f.certifyOn(f.s, f.matchedRun(def, cleanPops()).RunID, domain.CertifyRequest{})
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	if cert.ManifestSerialization == nil || *cert.ManifestSerialization != domain.CertificationManifestSerialization {
		t.Fatalf("manifest serialization = %v, want %s", cert.ManifestSerialization, domain.CertificationManifestSerialization)
	}
	for _, p := range cert.Populations {
		if p.HashAlgorithm != domain.PopulationHashAlgorithm || p.HashSerialization != domain.PopulationHashSerialization {
			t.Fatalf("certified population %s records %q/%q", p.Side, p.HashAlgorithm, p.HashSerialization)
		}
	}
	got, err := f.s.GetCertification(f.ctx, orgA, cert.RunID)
	if err != nil || got.ManifestSerialization == nil {
		t.Fatalf("GetCertification did not return the manifest serialization: %+v (err %v)", got, err)
	}
}
