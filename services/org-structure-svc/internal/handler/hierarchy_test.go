package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/handler"
)

// snapshotCall is what the handler asked the store for.
type snapshotCall struct {
	entity, asOf, recordedAsOf string
	includeInactive            bool
}

func (s *stubStore) HierarchySnapshot(_ context.Context, entity, asOf, recordedAsOf string, includeInactive bool) ([]domain.OrgUnit, error) {
	s.lastSnap = snapshotCall{entity, asOf, recordedAsOf, includeInactive}
	out := []domain.OrgUnit{}
	for _, u := range s.units {
		if u.LegalEntityID == entity && (includeInactive || u.Status != domain.UnitStatusInactive) {
			out = append(out, *u)
		}
	}
	return out, nil
}

func getHierarchy(t *testing.T, r chi.Router, query string) (int, handler.HierarchyResponse, string) {
	t.Helper()
	rr := doReqH(r, http.MethodGet, "/v1/org/hierarchy?"+query)
	var out handler.HierarchyResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out, rr.Body.String()
}

func TestHierarchy_ReturnsNestedTree(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	root, mid, _ := chain(t, r)
	_ = mid

	code, out, body := getHierarchy(t, r, "legal_entity_id=le-us")
	if code != 200 {
		t.Fatalf("got %d %s", code, body)
	}
	if len(out.Roots) != 1 || out.Roots[0].UnitID != root {
		t.Fatalf("one root expected, got %+v", out.Roots)
	}
	m := out.Roots[0].Children
	if len(m) != 1 || m[0].Code != "MID" || len(m[0].Children) != 1 || m[0].Children[0].Code != "LEAF" {
		t.Fatalf("root > MID > LEAF expected, got %s", body)
	}
	if out.LegalEntityID != "le-us" || out.AsOf == "" || out.IncludeInactive {
		t.Fatalf("a current read echoes today and excludes retired units: %+v", out)
	}
}

func TestHierarchy_EmptyEntityIsEmptyList(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	code, out, body := getHierarchy(t, r, "legal_entity_id=le-none")
	if code != 200 || out.Roots == nil || len(out.Roots) != 0 {
		t.Fatalf("want 200 with roots [], got %d %s", code, body)
	}
}

func TestHierarchy_PassesDatesThroughToTheStore(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	code, out, _ := getHierarchy(t, r, "legal_entity_id=le-us&as_of=2026-02-01&recorded_as_of=2026-02-02T10:00:00Z")
	if code != 200 {
		t.Fatalf("got %d", code)
	}
	want := snapshotCall{"le-us", "2026-02-01", "2026-02-02T10:00:00Z", true}
	if s.lastSnap != want {
		t.Fatalf("store asked for %+v, want %+v", s.lastSnap, want)
	}
	if out.AsOf != "2026-02-01" || out.RecordedAsOf != "2026-02-02T10:00:00Z" {
		t.Fatalf("response must echo the dates: %+v", out)
	}
}

// A dated read includes retired units by default (the past tree had them);
// a current read does not; either default can be overridden.
func TestHierarchy_IncludeInactiveDefaultsAndOverride(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	keep := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("KEEP", "k-1", nil), "p1")).UnitID
	gone := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("GONE", "k-2", nil), "p1")).UnitID
	transition(r, gone, "activate", "1")
	transition(r, gone, "deactivate", "2")

	count := func(query string) int {
		_, out, _ := getHierarchy(t, r, query)
		return len(out.Roots)
	}
	if n := count("legal_entity_id=le-us"); n != 1 {
		t.Fatalf("current read hides the retired unit: got %d roots", n)
	}
	if n := count("legal_entity_id=le-us&as_of=2026-02-01"); n != 2 {
		t.Fatalf("dated read keeps the retired unit: got %d roots", n)
	}
	if n := count("legal_entity_id=le-us&include_inactive=true"); n != 2 {
		t.Fatalf("explicit include_inactive=true: got %d roots", n)
	}
	if n := count("legal_entity_id=le-us&as_of=2026-02-01&include_inactive=false"); n != 1 {
		t.Fatalf("explicit include_inactive=false wins over the dated default: got %d roots", n)
	}
	_ = keep
}

// A child whose parent was left out of the snapshot is still returned, as a root.
func TestHierarchy_ChildOfExcludedParentIsNotLost(t *testing.T) {
	s := newStubStore()
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	parent := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("P", "k-1", nil), "p1")).UnitID
	kid := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("KID", "k-2", &parent), "p1")).UnitID
	s.units[parent].Status = domain.UnitStatusInactive // as if retired, child still live

	_, out, body := getHierarchy(t, r, "legal_entity_id=le-us")
	if len(out.Roots) != 1 || out.Roots[0].UnitID != kid {
		t.Fatalf("the child must surface as a root, got %s", body)
	}
}

func TestHierarchy_Validation(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	cases := []struct {
		query string
		code  int
		err   string
	}{
		{"", 400, "missing_fields"},
		{"legal_entity_id=le-us&as_of=tomorrow", 400, "invalid_as_of"},
		{"legal_entity_id=le-us&recorded_as_of=2026-02-02", 400, "invalid_recorded_as_of"},
		{"legal_entity_id=le-us&include_inactive=maybe", 400, "invalid_include_inactive"},
	}
	for _, c := range cases {
		rr := doReqH(r, http.MethodGet, "/v1/org/hierarchy?"+c.query)
		if rr.Code != c.code || errCode(rr) != c.err {
			t.Errorf("%q: want %d %s, got %d %s", c.query, c.code, c.err, rr.Code, rr.Body.String())
		}
	}
	if rr := doReq(r, http.MethodGet, "/v1/org/hierarchy?legal_entity_id=le-us", nil, ""); rr.Code != 401 {
		t.Fatalf("no principal: want 401, got %d", rr.Code)
	}
}

func TestHierarchy_AuthzDeniedReadsNothing(t *testing.T) {
	s := newStubStore()
	allow := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	chain(t, allow)

	deny := newRouter(s, &stubPublisher{}, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &stubEmployeeValidator{})
	s.lastSnap = snapshotCall{}
	if code, _, _ := getHierarchy(t, deny, "legal_entity_id=le-us"); code != 403 {
		t.Fatalf("denied: want 403, got %d", code)
	}
	if s.lastSnap != (snapshotCall{}) {
		t.Fatal("a denied caller must not reach the store")
	}
}

// Stored data with a cycle is reported, not silently trimmed.
func TestHierarchy_CorruptCycleIsAnError(t *testing.T) {
	s := newStubStore()
	a, b := "a", "b"
	s.units["a"] = &domain.OrgUnit{UnitID: "a", LegalEntityID: "le-us", Code: "A", ParentUnitID: &b, Status: domain.UnitStatusActive}
	s.units["b"] = &domain.OrgUnit{UnitID: "b", LegalEntityID: "le-us", Code: "B", ParentUnitID: &a, Status: domain.UnitStatusActive}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReqH(r, http.MethodGet, "/v1/org/hierarchy?legal_entity_id=le-us")
	if rr.Code != 500 || errCode(rr) != "hierarchy_inconsistent" {
		t.Fatalf("want 500 hierarchy_inconsistent, got %d %s", rr.Code, rr.Body.String())
	}
}

func doReqH(r chi.Router, method, path string) *httptest.ResponseRecorder {
	return doReq(r, method, path, nil, "p1")
}
