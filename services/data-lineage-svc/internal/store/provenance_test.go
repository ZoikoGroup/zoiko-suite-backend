package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"zoiko.io/data-lineage-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-04 / TC-09 / NP-31..NP-34 / NP-39 (docs/architecture/DQC_GAP_MATRIX.md, step B).
//
// Before this change an edge said only "source -> activity -> derived": no run,
// no build, no configuration digest, no input/output version — and sealing a
// manifest for an entity with no inputs at all succeeded. These run against
// real Postgres as the NOSUPERUSER NOBYPASSRLS role like the rest of the suite.

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// derive records a derivation src -> dst with the given provenance and returns the edge.
func (f *fixture) derive(src, dst string, p domain.ProvenanceFields) *domain.LineageEdge {
	f.t.Helper()
	return f.recordDerivation(orgA, domain.RecordDerivationRequest{
		SourceEntity: ref("landing_object", src), ActivityType: "transform",
		TransformationVersion: strp("svc:Transform:v1"), Agent: "test-agent",
		DerivedEntity: ref("report_row", dst), ProvenanceFields: p,
	})
}

func (f *fixture) seal(entityID string) (*domain.ProvenanceManifest, error) {
	f.t.Helper()
	return f.s.SealManifest(f.ctx, orgA, entityID, "test-operator", f.claim("SealManifest", entityID))
}

// NP-31: an entity with no upstream lineage has nothing to certify.
func TestSeal_EntityWithNoInputLineage_IsRefused(t *testing.T) {
	f := newFixture(t)
	edge := f.derive("np31-src", "np31-dst", fullProv("run-np31"))

	// The SOURCE has no upstream edges at all. Sealing it used to succeed with
	// an empty graph — a hash of nothing, indistinguishable from "never recorded".
	if _, err := f.seal(edge.SourceEntityID); !errors.Is(err, domain.ErrNoInputLineage) {
		t.Fatalf("err = %v, want ErrNoInputLineage", err)
	}
	// The derived entity, which does have lineage, still seals.
	if _, err := f.seal(edge.DerivedEntityID); err != nil {
		t.Fatalf("seal entity with complete lineage: %v", err)
	}
}

// NP-33 / NP-34 / TC-09: each missing piece of run provenance blocks sealing on its own.
func TestSeal_EachMissingRunProvenanceField_BlocksSealing(t *testing.T) {
	cases := []struct {
		name string
		drop func(p *domain.ProvenanceFields)
	}{
		{"run_id", func(p *domain.ProvenanceFields) { p.RunID = nil }},
		{"build_version", func(p *domain.ProvenanceFields) { p.BuildVersion = nil }},
		{"config_digest", func(p *domain.ProvenanceFields) { p.ConfigDigest = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p := fullProv("run-" + tc.name)
			tc.drop(&p)
			edge := f.derive("miss-src-"+tc.name, "miss-dst-"+tc.name, p)

			_, err := f.seal(edge.DerivedEntityID)
			if !errors.Is(err, domain.ErrMissingRunProvenance) {
				t.Fatalf("sealed despite no %s: err = %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("error should name the missing field %q: %v", tc.name, err)
			}
		})
	}
}

// NP-32: an edge must pin exact input and output versions.
func TestSeal_EdgeWithoutVersionPins_IsRefused(t *testing.T) {
	for _, field := range []string{"source_version", "derived_version"} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			p := fullProv("run-pin-" + field)
			if field == "source_version" {
				p.SourceVersion = nil
			} else {
				p.DerivedVersion = nil
			}
			edge := f.derive("pin-src-"+field, "pin-dst-"+field, p)
			_, err := f.seal(edge.DerivedEntityID)
			if !errors.Is(err, domain.ErrMissingVersionPin) {
				t.Fatalf("sealed despite no %s: err = %v", field, err)
			}
		})
	}
}

// The sealed manifest carries the provenance, so it is evidence of WHICH run,
// build, configuration and versions — not just of a link.
func TestSeal_ManifestCarriesRunProvenanceAndPins(t *testing.T) {
	f := newFixture(t)
	p := fullProv("run-evidence")
	edge := f.derive("ev-src", "ev-dst", p)
	m, err := f.seal(edge.DerivedEntityID)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(m.Manifest.Activities) != 1 || len(m.Manifest.Edges) != 1 {
		t.Fatalf("snapshot shape: %+v", m.Manifest)
	}
	a, e := m.Manifest.Activities[0], m.Manifest.Edges[0]
	if a.RunID == nil || *a.RunID != "run-evidence" || a.BuildVersion == nil || *a.BuildVersion != *p.BuildVersion ||
		a.ConfigDigest == nil || *a.ConfigDigest != *p.ConfigDigest {
		t.Fatalf("activity provenance not in manifest: %+v", a)
	}
	if e.SourceVersion == nil || *e.SourceVersion != "src@wm-100" || e.DerivedVersion == nil || *e.DerivedVersion != "out@v1" {
		t.Fatalf("version pins not in manifest: %+v", e)
	}
}

// NP-32: a moving pointer is not a version. Refused by the application...
func TestRecord_MutablePointerIsNotAVersionPin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(p *domain.ProvenanceFields)
	}{
		{"source latest", func(p *domain.ProvenanceFields) { p.SourceVersion = strp("latest") }},
		{"derived HEAD (any case)", func(p *domain.ProvenanceFields) { p.DerivedVersion = strp("HEAD") }},
		{"derived with whitespace", func(p *domain.ProvenanceFields) { p.DerivedVersion = strp("  current ") }},
		{"build main", func(p *domain.ProvenanceFields) { p.BuildVersion = strp("main") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			p := fullProv("run-mut")
			tc.mutate(&p)
			_, err := f.s.RecordDerivation(f.ctx, orgA, domain.RecordDerivationRequest{
				SourceEntity: ref("landing_object", "mut-src"), ActivityType: "transform",
				TransformationVersion: strp("v1"), Agent: "a", DerivedEntity: ref("report_row", "mut-dst"), ProvenanceFields: p,
			}, "test-operator", f.claim("RecordDerivation", "mut"))
			if err == nil || !strings.Contains(err.Error(), "moving target") {
				t.Fatalf("recorded a mutable pointer as a pin: err = %v", err)
			}
		})
	}
}

// ...and by the database, for any other writer.
func TestDatabase_RefusesAMutablePointerPin(t *testing.T) {
	f := newFixture(t)
	edge := f.derive("db-src", "db-dst", fullProv("run-db"))
	_, err := f.admin.Exec(f.ctx, `
		INSERT INTO lineage_edges (edge_id, tenant_id, source_entity_id, activity_id, derived_entity_id, created_by, source_version, derived_version)
		VALUES ('dlg_00000000-0000-0000-0000-0000000000aa', $1, $2, $3, $4, 'raw-writer', 'v1', 'LATEST')`,
		orgA, edge.SourceEntityID, edge.ActivityID, edge.DerivedEntityID)
	if err == nil {
		t.Fatal("database accepted derived_version = 'LATEST'")
	}
	// Prove it is THAT constraint, not some other failure: the same row with an exact pin is accepted.
	if _, err := f.admin.Exec(f.ctx, `
		INSERT INTO lineage_edges (edge_id, tenant_id, source_entity_id, activity_id, derived_entity_id, created_by, source_version, derived_version)
		VALUES ('dlg_00000000-0000-0000-0000-0000000000ab', $1, $2, $3, $4, 'raw-writer', 'v1', 'v2')`,
		orgA, edge.SourceEntityID, edge.ActivityID, edge.DerivedEntityID); err != nil {
		t.Fatalf("database refused a well-formed edge: %v", err)
	}
}

func TestRecord_MalformedConfigDigest_IsRefused(t *testing.T) {
	for _, bad := range []string{"abc", strings.ToUpper(digest("x")), digest("x") + "0", strings.Repeat("g", 64)} {
		f := newFixture(t)
		p := fullProv("run-dig")
		p.ConfigDigest = strp(bad)
		_, err := f.s.RecordDerivation(f.ctx, orgA, domain.RecordDerivationRequest{
			SourceEntity: ref("landing_object", "dg-src"), ActivityType: "t", TransformationVersion: strp("v1"),
			Agent: "a", DerivedEntity: ref("report_row", "dg-dst"), ProvenanceFields: p,
		}, "test-operator", f.claim("RecordDerivation", "dg"))
		if err == nil || !strings.Contains(err.Error(), "config_digest") {
			t.Errorf("config_digest %q accepted: err = %v", bad, err)
		}
	}
}

// INV-23: running the same transformation again is a NEW run and a new edge —
// it never overwrites or collapses into the earlier one.
func TestRecord_ARerunIsANewEdge_NotACollapse(t *testing.T) {
	f := newFixture(t)
	e1 := f.derive("rr-src", "rr-dst", fullProv("run-1"))
	e2 := f.derive("rr-src", "rr-dst", fullProv("run-2"))
	if e1.EdgeID == e2.EdgeID || e1.ActivityID == e2.ActivityID {
		t.Fatalf("a second run collapsed into the first: %s / %s", e1.EdgeID, e2.EdgeID)
	}
	// The same run recorded twice is still a retry, not a second edge.
	e1again := f.derive("rr-src", "rr-dst", fullProv("run-1"))
	if e1again.EdgeID != e1.EdgeID {
		t.Fatalf("a retry of the same run created a second edge: %s vs %s", e1again.EdgeID, e1.EdgeID)
	}
}

// One run_id cannot have run two builds or two configurations.
func TestRecord_SameRunDifferentConfig_IsAConflict(t *testing.T) {
	f := newFixture(t)
	f.derive("cf-src", "cf-dst", fullProv("run-cf"))

	other := fullProv("run-cf")
	other.ConfigDigest = strp(digest("a different configuration"))
	_, err := f.s.RecordDerivation(f.ctx, orgA, domain.RecordDerivationRequest{
		SourceEntity: ref("landing_object", "cf-src"), ActivityType: "transform", TransformationVersion: strp("svc:Transform:v1"),
		Agent: "test-agent", DerivedEntity: ref("report_row", "cf-dst"), ProvenanceFields: other,
	}, "test-operator", f.claim("RecordDerivation", "cf"))
	if !errors.Is(err, domain.ErrRunProvenanceConflict) {
		t.Fatalf("err = %v, want ErrRunProvenanceConflict", err)
	}
}

// NP-39: history is not edited. The activity row is sealed at the database.
func TestDatabase_ActivityIsImmutable(t *testing.T) {
	f := newFixture(t)
	edge := f.derive("im-src", "im-dst", fullProv("run-im"))

	if _, err := f.admin.Exec(f.ctx, `UPDATE lineage_activities SET run_id = 'rewritten' WHERE activity_id = $1`, edge.ActivityID); err == nil {
		t.Fatal("database allowed rewriting an activity's run_id")
	}
	if _, err := f.admin.Exec(f.ctx, `DELETE FROM lineage_activities WHERE activity_id = $1`, edge.ActivityID); err == nil {
		t.Fatal("database allowed deleting an activity")
	}
	var run string
	if err := f.admin.QueryRow(f.ctx, `SELECT run_id FROM lineage_activities WHERE activity_id = $1`, edge.ActivityID).Scan(&run); err != nil || run != "run-im" {
		t.Fatalf("activity run_id = %q (err %v), want it untouched", run, err)
	}
}

// The repair path. A legacy edge (recorded with no provenance) can never seal,
// and superseding it with the SAME activity would inherit the gap forever.
// Superseding with replacement provenance writes a NEW activity, leaves the old
// one untouched, and makes the graph sealable.
func TestSupersede_WithReplacementProvenance_MakesALegacyGraphSealable(t *testing.T) {
	f := newFixture(t)
	legacy := f.derive("lg-src", "lg-dst", domain.ProvenanceFields{}) // no provenance at all
	if _, err := f.seal(legacy.DerivedEntityID); !errors.Is(err, domain.ErrMissingRunProvenance) {
		t.Fatalf("legacy graph sealed: err = %v", err)
	}

	p := fullProv("run-backfill")
	repl, err := f.s.SupersedeLineage(f.ctx, orgA, domain.SupersedeLineageRequest{
		EdgeID: legacy.EdgeID, Reason: "backfilled run provenance from the job scheduler's records",
		ProvenanceFields: p,
	}, "test-operator", f.claim("SupersedeLineage", legacy.EdgeID))
	if err != nil {
		t.Fatalf("supersede with replacement: %v", err)
	}
	if repl.ActivityID == legacy.ActivityID {
		t.Fatal("replacement reused the old activity — it would have inherited the gap")
	}
	if _, err := f.seal(legacy.DerivedEntityID); err != nil {
		t.Fatalf("repaired graph still refuses to seal: %v", err)
	}

	// History intact: the original activity still has NO run id, exactly as recorded.
	var run *string
	if err := f.admin.QueryRow(f.ctx, `SELECT run_id FROM lineage_activities WHERE activity_id = $1`, legacy.ActivityID).Scan(&run); err != nil {
		t.Fatalf("read original activity: %v", err)
	}
	if run != nil {
		t.Fatalf("the original activity was edited: run_id = %q", *run)
	}
}

// Without replacement, supersession keeps the old activity and carries the pins.
func TestSupersede_WithoutReplacement_InheritsActivityAndPins(t *testing.T) {
	f := newFixture(t)
	orig := f.derive("in-src", "in-dst", fullProv("run-in"))
	next, err := f.s.SupersedeLineage(f.ctx, orgA, domain.SupersedeLineageRequest{
		EdgeID: orig.EdgeID, Reason: "re-pointed",
	}, "test-operator", f.claim("SupersedeLineage", orig.EdgeID))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if next.ActivityID != orig.ActivityID {
		t.Fatalf("activity changed without a replacement: %s -> %s", orig.ActivityID, next.ActivityID)
	}
	if next.SourceVersion == nil || *next.SourceVersion != "src@wm-100" || next.DerivedVersion == nil || *next.DerivedVersion != "out@v1" {
		t.Fatalf("pins were not carried onto the superseding edge: %+v", next)
	}
	if _, err := f.seal(orig.DerivedEntityID); err != nil {
		t.Fatalf("seal after a plain supersede: %v", err)
	}
}

// A replacement can correct one field and inherit the rest.
func TestSupersede_PartialReplacement_OverlaysOnlyWhatIsGiven(t *testing.T) {
	f := newFixture(t)
	orig := f.derive("pr-src", "pr-dst", fullProv("run-pr"))
	newDigest := digest("corrected configuration")
	next, err := f.s.SupersedeLineage(f.ctx, orgA, domain.SupersedeLineageRequest{
		EdgeID: orig.EdgeID, Reason: "config digest was recorded from the wrong file",
		ProvenanceFields: domain.ProvenanceFields{ConfigDigest: strp(newDigest)},
	}, "test-operator", f.claim("SupersedeLineage", orig.EdgeID))
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	m, err := f.seal(next.DerivedEntityID)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(m.Manifest.Activities) != 1 {
		t.Fatalf("manifest should hold only the current activity, has %d", len(m.Manifest.Activities))
	}
	a := m.Manifest.Activities[0]
	if *a.ConfigDigest != newDigest {
		t.Fatalf("config digest not corrected: %s", *a.ConfigDigest)
	}
	if *a.RunID != "run-pr" || *a.BuildVersion != "build-2026.11.23+abc123" {
		t.Fatalf("fields that were not corrected were not inherited: %+v", a)
	}
}
