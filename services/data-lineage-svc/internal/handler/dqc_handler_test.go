package handler_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"zoiko.io/data-lineage-svc/internal/domain"
	"zoiko.io/data-lineage-svc/internal/store"
)

// GET /dqc/lineage: a read-only projection that says what is missing from the provenance.

type graphStore struct {
	store.Store
	calls     int
	direction string
	asOf      time.Time
	snap      *domain.LineageGraphSnapshot
	err       error
}

func (s *graphStore) GetUpstreamLineage(_ context.Context, _, id string) (*domain.LineageGraphSnapshot, error) {
	s.calls++
	s.direction = "upstream"
	return s.snap, s.err
}

func (s *graphStore) GetDownstreamImpact(_ context.Context, _, id string) (*domain.LineageGraphSnapshot, error) {
	s.calls++
	s.direction = "downstream"
	return s.snap, s.err
}

func (s *graphStore) GetAsOfLineage(_ context.Context, _, id string, asOf time.Time) (*domain.LineageGraphSnapshot, error) {
	s.calls++
	s.direction = "as-of"
	s.asOf = asOf
	return s.snap, s.err
}

func str(s string) *string { return &s }

func completeGraph() *domain.LineageGraphSnapshot {
	return &domain.LineageGraphSnapshot{
		RootEntityID: "ent_1",
		Activities:   []domain.LineageActivity{{ActivityID: "act_1", TransformationVersion: str("t:v1"), RunID: str("r"), BuildVersion: str("b"), ConfigDigest: str("d")}},
		Edges:        []domain.LineageEdge{{EdgeID: "edge_1", ActivityID: "act_1", SourceVersion: str("s@1"), DerivedVersion: str("o@1")}},
	}
}

const dqcPath = "/v1/data-lineage/dqc/lineage"

func TestDQCLineage_DefaultsToUpstreamAndReportsCompleteness(t *testing.T) {
	st := &graphStore{snap: completeGraph()}
	rec := do(t, newServer(t, st, newAuthz(t, 200, "GRANTED")), "GET", dqcPath+"?entity_id=ent_1", "", nil)
	body := rec.Body.String()
	if rec.Code != 200 || st.direction != "upstream" || !strings.Contains(body, `"complete":true`) || !strings.Contains(body, `"gaps":[]`) {
		t.Fatalf("status %d direction %q body %s", rec.Code, st.direction, body)
	}
}

func TestDQCLineage_NamesTheGapsInAnIncompleteGraph(t *testing.T) {
	g := completeGraph()
	g.Activities[0].RunID = nil
	g.Edges[0].DerivedVersion = nil
	st := &graphStore{snap: g}
	rec := do(t, newServer(t, st, newAuthz(t, 200, "GRANTED")), "GET", dqcPath+"?entity_id=ent_1", "", nil)
	body := rec.Body.String()
	for _, want := range []string{`"complete":false`, `"kind":"MISSING_RUN_ID"`, `"activity_id":"act_1"`, `"kind":"MISSING_DERIVED_VERSION"`, `"edge_id":"edge_1"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}

func TestDQCLineage_DirectionAndAsOf(t *testing.T) {
	st := &graphStore{snap: completeGraph()}
	srv := newServer(t, st, newAuthz(t, 200, "GRANTED"))
	if rec := do(t, srv, "GET", dqcPath+"?entity_id=e&direction=downstream", "", nil); rec.Code != 200 || st.direction != "downstream" {
		t.Fatalf("downstream: %d %q", rec.Code, st.direction)
	}
	if rec := do(t, srv, "GET", dqcPath+"?entity_id=e&as_of=1767225600", "", nil); rec.Code != 200 || st.direction != "as-of" || st.asOf.Unix() != 1767225600 {
		t.Fatalf("as-of: %d %q %v", rec.Code, st.direction, st.asOf)
	}
	before := st.calls
	for name, q := range map[string]string{
		"no entity":         "",
		"unknown direction": "?entity_id=e&direction=sideways",
		"bad as_of":         "?entity_id=e&as_of=yesterday",
		"downstream as_of":  "?entity_id=e&direction=downstream&as_of=1767225600",
	} {
		if rec := do(t, srv, "GET", dqcPath+q, "", nil); rec.Code != 400 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	if st.calls != before {
		t.Fatalf("a malformed request reached the store %d times", st.calls-before)
	}
}

func TestDQCLineage_NeedsReadPermissionAndMapsNotFound(t *testing.T) {
	st := &graphStore{snap: completeGraph()}
	if rec := do(t, newServer(t, st, newAuthz(t, 200, "DENIED")), "GET", dqcPath+"?entity_id=e", "", nil); rec.Code != 403 || st.calls != 0 {
		t.Fatalf("denied: %d, store calls %d", rec.Code, st.calls)
	}
	az := newAuthz(t, 200, "GRANTED")
	do(t, newServer(t, st, az), "GET", dqcPath+"?entity_id=e", "", nil)
	if !strings.Contains(az.lastBody(), "DATA_LINEAGE_READ") {
		t.Fatalf("the action checked was not DATA_LINEAGE_READ: %s", az.lastBody())
	}
	nf := &graphStore{err: domain.ErrLineageEntityNotFound}
	if rec := do(t, newServer(t, nf, newAuthz(t, 200, "GRANTED")), "GET", dqcPath+"?entity_id=missing", "", nil); rec.Code != 404 {
		t.Fatalf("unknown entity: %d", rec.Code)
	}
}
