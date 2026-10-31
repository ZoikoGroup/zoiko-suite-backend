package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/org-structure-svc/internal/domain"
)

// ── stub reparent: same rules as PgStore.ReparentOrgUnit, in memory ────────────

func (s *stubStore) ReparentOrgUnit(_ context.Context, id string, req *domain.ReparentOrgUnitRequest, expected int64, _ string) (*domain.OrgUnit, error) {
	u, ok := s.units[id]
	if !ok {
		return nil, domain.ErrOrgUnitNotFound
	}
	if u.RecordVersion != expected {
		return nil, domain.ErrVersionConflict
	}
	if u.Status == domain.UnitStatusInactive {
		return nil, domain.ErrInvalidTransition
	}
	if req.NewParentUnitID != nil {
		p, ok := s.units[*req.NewParentUnitID]
		if !ok || p.LegalEntityID != u.LegalEntityID {
			return nil, domain.ErrParentInvalid
		}
		if p.Status == domain.UnitStatusInactive || u.Status == domain.UnitStatusActive && p.Status != domain.UnitStatusActive {
			return nil, domain.ErrParentNotActive
		}
		parentOf := func(x string) *string { return s.units[x].ParentUnitID }
		if domain.ReachesUnit(*req.NewParentUnitID, id, parentOf) {
			return nil, domain.ErrHierarchyLoop
		}
	}
	same := u.ParentUnitID == nil && req.NewParentUnitID == nil ||
		u.ParentUnitID != nil && req.NewParentUnitID != nil && *u.ParentUnitID == *req.NewParentUnitID
	if same {
		return nil, domain.ErrNoHierarchyChange
	}
	if req.EffectiveAt <= s.placeFrom[id] { // ISO dates compare as strings
		return nil, domain.ErrEffectiveDateInvalid
	}
	u.ParentUnitID = req.NewParentUnitID
	s.placeFrom[id] = req.EffectiveAt
	u.RecordVersion++
	return u, nil
}

// ── helpers ────────────────────────────────────────────────────────────────────

func reparent(r http.Handler, id, ifMatch string, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/org/units/"+id+"/reparent", strings.NewReader(string(b)))
	req.Header.Set("X-Principal-Id", "p1")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func move(parent *string, at string) map[string]any {
	b := map[string]any{"effective_at": at, "reason": "reorganisation"}
	if parent != nil {
		b["new_parent_unit_id"] = *parent
	}
	return b
}

func strPtr(s string) *string { return &s }

// chain builds root <- mid <- leaf and returns their ids.
func chain(t *testing.T, r chi.Router) (root, mid, leaf string) {
	t.Helper()
	root = decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("ROOT", "k-1", nil), "p1")).UnitID
	mid = decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("MID", "k-2", &root), "p1")).UnitID
	leaf = decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("LEAF", "k-3", &mid), "p1")).UnitID
	return
}

// ── tests ──────────────────────────────────────────────────────────────────────

func TestReparent_MovesUnitAndBumpsVersion(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubEmployeeValidator{})
	root, _, leaf := chain(t, r)
	before := pub.orgChanged

	rr := reparent(r, leaf, "1", move(&root, "2026-03-01"))
	if rr.Code != 200 {
		t.Fatalf("reparent: got %d %s", rr.Code, rr.Body.String())
	}
	u := decodeUnit(t, rr)
	if u.ParentUnitID == nil || *u.ParentUnitID != root || u.RecordVersion != 2 {
		t.Fatalf("leaf should now be under root at v2, got parent=%v v%d", u.ParentUnitID, u.RecordVersion)
	}
	if pub.orgChanged != before+1 {
		t.Fatalf("reparent must publish once, got %d", pub.orgChanged-before)
	}
}

func TestReparent_CanMakeAUnitARoot(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	_, mid, _ := chain(t, r)

	rr := reparent(r, mid, "1", move(nil, "2026-03-01"))
	if rr.Code != 200 || decodeUnit(t, rr).ParentUnitID != nil {
		t.Fatalf("make root: got %d %s", rr.Code, rr.Body.String())
	}
}

// Spec negative test 9: a reparent that creates a hierarchy loop is rejected.
func TestReparent_RejectsLoops(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	root, mid, leaf := chain(t, r)

	for name, c := range map[string]struct{ unit, under string }{
		"under its own grandchild": {root, leaf},
		"under its own child":      {root, mid},
		"under itself":             {mid, mid},
	} {
		under := c.under
		rr := reparent(r, c.unit, "1", move(&under, "2026-03-01"))
		if rr.Code != 422 || errCode(rr) != "hierarchy_loop" {
			t.Errorf("%s: want 422 hierarchy_loop, got %d %s", name, rr.Code, rr.Body.String())
		}
	}
	// A refused move changes nothing, so the version is still 1.
	if rr := reparent(r, root, "1", move(&leaf, "2026-03-01")); rr.Code != 422 {
		t.Fatalf("a refused move must not consume the version: got %d", rr.Code)
	}
}

func TestReparent_Validation(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	root, mid, leaf := chain(t, r)

	cases := []struct {
		name    string
		unit    string
		ifMatch string
		body    map[string]any
		code    int
		err     string
	}{
		{"no If-Match", leaf, "", move(&root, "2026-03-01"), 428, "if_match_required"},
		{"stale version", leaf, "5", move(&root, "2026-03-01"), 409, "version_conflict"},
		{"already under that parent", leaf, "1", move(&mid, "2026-03-01"), 409, "no_change"},
		{"effective on the placement start", leaf, "1", move(&root, "2026-01-01"), 422, "invalid_effective_date"},
		{"effective before the placement start", leaf, "1", move(&root, "2025-06-01"), 422, "invalid_effective_date"},
		{"bad date", leaf, "1", move(&root, "March"), 400, "invalid_effective_at"},
		{"unknown unit", "nope", "1", move(&root, "2026-03-01"), 404, "org_unit_not_found"},
		{"unknown parent", leaf, "1", move(strPtr("ghost"), "2026-03-01"), 422, "parent_invalid"},
		{"no reason", leaf, "1", map[string]any{"effective_at": "2026-03-01", "new_parent_unit_id": root}, 400, "missing_fields"},
	}
	for _, c := range cases {
		rr := reparent(r, c.unit, c.ifMatch, c.body)
		if rr.Code != c.code || errCode(rr) != c.err {
			t.Errorf("%s: want %d %s, got %d %s", c.name, c.code, c.err, rr.Code, rr.Body.String())
		}
	}
}

func TestReparent_ActiveUnitNeedsActiveParent(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	root, mid, _ := chain(t, r)
	other := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("OTHER", "k-9", nil), "p1")).UnitID
	transition(r, root, "activate", "1")
	transition(r, mid, "activate", "1")

	// mid is ACTIVE; OTHER is still DRAFT, so mid cannot move under it.
	if rr := reparent(r, mid, "2", move(&other, "2026-03-01")); rr.Code != 409 || errCode(rr) != "parent_not_active" {
		t.Fatalf("active unit under draft parent: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestReparent_AuthzDeniedAndInactiveUnit(t *testing.T) {
	s := newStubStore()
	allow := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	root, mid, leaf := chain(t, allow)

	deny := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubEmployeeValidator{})
	if rr := reparent(deny, leaf, "1", move(&root, "2026-03-01")); rr.Code != 403 {
		t.Fatalf("denied: want 403, got %d", rr.Code)
	}
	if *s.units[leaf].ParentUnitID != mid {
		t.Fatal("a denied reparent must not move the unit")
	}

	transition(allow, root, "activate", "1")
	transition(allow, mid, "activate", "1")
	transition(allow, leaf, "activate", "1")
	transition(allow, leaf, "deactivate", "2")
	if rr := reparent(allow, leaf, "3", move(&root, "2026-03-01")); rr.Code != 409 || errCode(rr) != "invalid_transition" {
		t.Fatalf("inactive unit: got %d %s", rr.Code, rr.Body.String())
	}
}
