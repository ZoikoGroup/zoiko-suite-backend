package store_test

import (
	"errors"
	"testing"

	"zoiko.io/data-lineage-svc/internal/domain"
)

// ZS-SVC-Z-001 DQC-04 (docs/architecture/DQC_GAP_MATRIX.md, step F3): the gaps a reader is shown are
// the gaps sealing refuses for, and dqc.lineage.recorded carries what the spec names.

// A graph is sealable if and only if it has no gaps: what GET /dqc/lineage reports and what sealing
// refuses can never disagree. (These graphs record no transformation version, which is itself a gap, so
// the only one that is gap-free is the one that supplies it.)
func TestLineageGaps_AgreeWithSealing(t *testing.T) {
	tv := "t:v1"
	cases := map[string]func(*domain.RecordDerivationRequest){
		"complete":       func(*domain.RecordDerivationRequest) {},
		"no transform":   func(r *domain.RecordDerivationRequest) { r.TransformationVersion = nil },
		"no run id":      func(r *domain.RecordDerivationRequest) { r.RunID = nil },
		"no build":       func(r *domain.RecordDerivationRequest) { r.BuildVersion = nil },
		"no config":      func(r *domain.RecordDerivationRequest) { r.ConfigDigest = nil },
		"no source pin":  func(r *domain.RecordDerivationRequest) { r.SourceVersion = nil },
		"no derived pin": func(r *domain.RecordDerivationRequest) { r.DerivedVersion = nil },
		"nothing at all": func(r *domain.RecordDerivationRequest) {
			r.ProvenanceFields = domain.ProvenanceFields{}
			r.TransformationVersion = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			req := domain.RecordDerivationRequest{
				SourceEntity: ref("landing_object", "gap-src-"+name), ActivityType: "ingest", Agent: "test", TransformationVersion: &tv,
				DerivedEntity: ref("landing_object", "gap-dst-"+name), ProvenanceFields: fullProv("run-gap"),
			}
			mutate(&req)
			edge := f.recordDerivation(orgA, req)
			snap, err := f.s.GetUpstreamLineage(f.ctx, orgA, edge.DerivedEntityID)
			if err != nil {
				t.Fatal(err)
			}
			gaps := domain.LineageGaps(snap)
			_, sealErr := f.seal(edge.DerivedEntityID)
			if (len(gaps) == 0) != (sealErr == nil) {
				t.Fatalf("gaps=%v but seal err=%v", gaps, sealErr)
			}
			if (name == "complete") != (len(gaps) == 0) {
				t.Fatalf("%s: gaps=%v", name, gaps)
			}
		})
	}
}

func TestLineageGaps_NoUpstreamIsAGapAndIsRefused(t *testing.T) {
	f := newFixture(t)
	tv := "t:v1"
	edge := f.recordDerivation(orgA, domain.RecordDerivationRequest{
		SourceEntity: ref("landing_object", "ok-src"), ActivityType: "ingest", Agent: "test", TransformationVersion: &tv,
		DerivedEntity: ref("landing_object", "ok-dst"), ProvenanceFields: fullProv("run-ok"),
	})
	empty, err := f.s.GetUpstreamLineage(f.ctx, orgA, edge.SourceEntityID)
	if err != nil {
		t.Fatal(err)
	}
	if g := domain.LineageGaps(empty); len(g) != 1 || g[0].Kind != domain.GapNoInputLineage {
		t.Fatalf("an entity with no upstream: %+v", g)
	}
	if _, err := f.seal(edge.SourceEntityID); !errors.Is(err, domain.ErrNoInputLineage) {
		t.Fatal(err)
	}
}

func TestLineageEvent_CarriesRunNodesAndDigests(t *testing.T) {
	f := newFixture(t)
	tv := "t:v1"
	p := fullProv("run-evt")
	edge := f.recordDerivation(orgA, domain.RecordDerivationRequest{
		SourceEntity: ref("landing_object", "evt-src"), ActivityType: "ingest", Agent: "test", TransformationVersion: &tv,
		DerivedEntity: ref("landing_object", "evt-dst"), ProvenanceFields: p,
	})
	var n int
	var run, in, out, cfg, build, inV, outV string
	if err := f.admin.QueryRow(f.ctx, `SELECT count(*), COALESCE(max(payload->>'run_id'),''), COALESCE(max(payload->>'input_node_id'),''),
			COALESCE(max(payload->>'output_node_id'),''), COALESCE(max(payload->>'config_digest'),''), COALESCE(max(payload->>'build_version'),''),
			COALESCE(max(payload->>'input_version'),''), COALESCE(max(payload->>'output_version'),'')
		FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'dqc.lineage.recorded'`, edge.EdgeID).Scan(&n, &run, &in, &out, &cfg, &build, &inV, &outV); err != nil {
		t.Fatal(err)
	}
	if n != 1 || run != "run-evt" || in != edge.SourceEntityID || out != edge.DerivedEntityID || cfg != *p.ConfigDigest || build != *p.BuildVersion ||
		inV != *p.SourceVersion || outV != *p.DerivedVersion {
		t.Fatalf("events=%d run=%q in=%q out=%q cfg=%q build=%q versions=%q/%q", n, run, in, out, cfg, build, inV, outV)
	}
	// The legacy event is still written, once.
	var legacy int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'DATA.LineageRecorded'`, edge.EdgeID).Scan(&legacy)
	if legacy != 1 {
		t.Fatalf("DATA.LineageRecorded written %d times", legacy)
	}
}

// Recording the same derivation again returns the existing edge and emits nothing new.
func TestLineageEvent_ADuplicateRecordingEmitsNoSecondEvent(t *testing.T) {
	f := newFixture(t)
	tv := "t:v1"
	req := domain.RecordDerivationRequest{
		SourceEntity: ref("landing_object", "dup-src"), ActivityType: "ingest", Agent: "test", TransformationVersion: &tv,
		DerivedEntity: ref("landing_object", "dup-dst"), ProvenanceFields: fullProv("run-dup"),
	}
	first := f.recordDerivation(orgA, req)
	second, err := f.s.RecordDerivation(f.ctx, orgA, req, "test-operator", f.claim("RecordDerivation", "dup-again"))
	if err != nil {
		t.Fatal(err)
	}
	if second.EdgeID != first.EdgeID {
		t.Fatalf("a duplicate recording made a new edge: %s vs %s", second.EdgeID, first.EdgeID)
	}
	var n int
	_ = f.admin.QueryRow(f.ctx, `SELECT count(*) FROM outbox_events WHERE event_type = 'dqc.lineage.recorded' AND payload->>'edge_id' = $1`, first.EdgeID).Scan(&n)
	if n != 1 {
		t.Fatalf("dqc.lineage.recorded written %d times for one edge", n)
	}
}
