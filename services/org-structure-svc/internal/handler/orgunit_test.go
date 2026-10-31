package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"zoiko.io/org-structure-svc/internal/domain"
)

// ── stub org-unit store: same rules as PgStore, in memory ──────────────────────

func (s *stubStore) CreateOrgUnit(_ context.Context, req *domain.CreateOrgUnitRequest, actorID string) (*domain.OrgUnit, bool, error) {
	for _, u := range s.units {
		if u.CorrelationID == req.CorrelationID {
			return u, false, nil
		}
		if u.LegalEntityID == req.LegalEntityID && u.UnitType == req.UnitType && u.Code == req.Code && u.Status != domain.UnitStatusInactive {
			return nil, false, domain.ErrOrgUnitCodeTaken
		}
	}
	if req.ParentUnitID != nil {
		p, ok := s.units[*req.ParentUnitID]
		if !ok || p.LegalEntityID != req.LegalEntityID {
			return nil, false, domain.ErrParentInvalid
		}
	}
	u := &domain.OrgUnit{
		UnitID: fmt.Sprintf("unit-%d", len(s.units)+1), TenantID: "tenant-abc", LegalEntityID: req.LegalEntityID,
		Code: req.Code, Name: req.Name, UnitType: req.UnitType, Status: domain.UnitStatusDraft,
		RecordVersion: 1, CorrelationID: req.CorrelationID, CreatedBy: actorID, ParentUnitID: req.ParentUnitID, ManagerEmployeeID: req.ManagerEmployeeID,
	}
	s.units[u.UnitID] = u
	s.placeFrom[u.UnitID] = req.EffectiveFrom
	if req.ManagerEmployeeID != nil {
		s.managerFrom[u.UnitID] = req.EffectiveFrom
	}
	return u, true, nil
}

func (s *stubStore) GetOrgUnit(_ context.Context, id, _ string) (*domain.OrgUnit, error) {
	u, ok := s.units[id]
	if !ok {
		return nil, domain.ErrOrgUnitNotFound
	}
	return u, nil
}

func (s *stubStore) ListOrgUnits(_ context.Context, legalEntityID, _ string) ([]domain.OrgUnit, error) {
	out := []domain.OrgUnit{}
	for _, u := range s.units {
		if legalEntityID == "" || u.LegalEntityID == legalEntityID {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (s *stubStore) TransitionOrgUnit(_ context.Context, id, command string, expected int64) (*domain.OrgUnit, error) {
	u, ok := s.units[id]
	if !ok {
		return nil, domain.ErrOrgUnitNotFound
	}
	if u.RecordVersion != expected {
		return nil, domain.ErrVersionConflict
	}
	next, err := domain.OrgUnitTransition(u.Status, command)
	if err != nil {
		return nil, err
	}
	switch command {
	case domain.UnitCommandActivate:
		if u.ParentUnitID != nil && s.units[*u.ParentUnitID].Status != domain.UnitStatusActive {
			return nil, domain.ErrParentNotActive
		}
	case domain.UnitCommandDeactivate:
		for _, c := range s.units {
			if c.ParentUnitID != nil && *c.ParentUnitID == id && c.Status != domain.UnitStatusInactive {
				return nil, domain.ErrHasActiveChildren
			}
		}
	}
	u.Status = next
	u.RecordVersion++
	return u, nil
}

// ── helpers ────────────────────────────────────────────────────────────────────

func unitBody(code, correlation string, parent *string) map[string]any {
	b := map[string]any{
		"legal_entity_id": "le-us", "code": code, "name": code + " unit", "unit_type": "DEPARTMENT",
		"effective_from": "2026-01-01", "correlation_id": correlation,
	}
	if parent != nil {
		b["parent_unit_id"] = *parent
	}
	return b
}

func decodeUnit(t *testing.T, rr *httptest.ResponseRecorder) domain.OrgUnit {
	t.Helper()
	var u domain.OrgUnit
	if err := json.Unmarshal(rr.Body.Bytes(), &u); err != nil {
		t.Fatalf("decode unit: %v (%s)", err, rr.Body.String())
	}
	return u
}

func errCode(rr *httptest.ResponseRecorder) string {
	var e map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &e)
	return e["error_code"]
}

func transition(r http.Handler, id, command, ifMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/org/units/"+id+"/"+command, strings.NewReader("{}"))
	req.Header.Set("X-Principal-Id", "p1")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

// ── tests ──────────────────────────────────────────────────────────────────────

func TestOrgUnit_CreateIsDraftAndIdempotent(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{}, &stubEmployeeValidator{})

	rr := doReq(r, http.MethodPost, "/v1/org/units", unitBody("FIN", "c-1", nil), "p1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	u := decodeUnit(t, rr)
	if u.Status != domain.UnitStatusDraft || u.RecordVersion != 1 {
		t.Fatalf("new unit must be DRAFT v1, got %s v%d", u.Status, u.RecordVersion)
	}

	again := doReq(r, http.MethodPost, "/v1/org/units", unitBody("FIN", "c-1", nil), "p1")
	if again.Code != http.StatusOK || decodeUnit(t, again).UnitID != u.UnitID {
		t.Fatalf("replay: want 200 with the original unit, got %d", again.Code)
	}
	if pub.orgChanged != 1 {
		t.Fatalf("replay must not publish again: %d events", pub.orgChanged)
	}
}

func TestOrgUnit_CreateValidation(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})

	bad := unitBody("X", "c-2", nil)
	bad["unit_type"] = "COST_CENTER"
	if rr := doReq(r, http.MethodPost, "/v1/org/units", bad, "p1"); rr.Code != 400 || errCode(rr) != "invalid_unit_type" {
		t.Fatalf("bad type: got %d %s", rr.Code, rr.Body.String())
	}
	bad = unitBody("X", "c-3", nil)
	bad["effective_from"] = "01/01/2026"
	if rr := doReq(r, http.MethodPost, "/v1/org/units", bad, "p1"); rr.Code != 400 || errCode(rr) != "invalid_effective_from" {
		t.Fatalf("bad date: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodPost, "/v1/org/units", unitBody("X", "c-4", nil), ""); rr.Code != 401 {
		t.Fatalf("no principal: want 401, got %d", rr.Code)
	}
}

func TestOrgUnit_DuplicateCodeAndBadParent(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	doReq(r, http.MethodPost, "/v1/org/units", unitBody("HR", "c-1", nil), "p1")

	if rr := doReq(r, http.MethodPost, "/v1/org/units", unitBody("HR", "c-2", nil), "p1"); rr.Code != 409 || errCode(rr) != "code_taken" {
		t.Fatalf("duplicate code: got %d %s", rr.Code, rr.Body.String())
	}
	missing := "no-such-unit"
	if rr := doReq(r, http.MethodPost, "/v1/org/units", unitBody("KID", "c-3", &missing), "p1"); rr.Code != 422 || errCode(rr) != "parent_invalid" {
		t.Fatalf("missing parent: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestOrgUnit_LifecycleDraftActiveInactive(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("OPS", "c-1", nil), "p1"))

	// Cannot deactivate a draft.
	if rr := transition(r, u.UnitID, "deactivate", "1"); rr.Code != 409 || errCode(rr) != "invalid_transition" {
		t.Fatalf("deactivate draft: got %d %s", rr.Code, rr.Body.String())
	}
	rr := transition(r, u.UnitID, "activate", `"1"`) // quoted ETag form is accepted
	if rr.Code != 200 || decodeUnit(t, rr).Status != domain.UnitStatusActive || decodeUnit(t, rr).RecordVersion != 2 {
		t.Fatalf("activate: got %d %s", rr.Code, rr.Body.String())
	}
	// An active unit cannot be activated again, and the old version is stale.
	if rr := transition(r, u.UnitID, "activate", "2"); rr.Code != 409 || errCode(rr) != "invalid_transition" {
		t.Fatalf("re-activate: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := transition(r, u.UnitID, "deactivate", "1"); rr.Code != 409 || errCode(rr) != "version_conflict" {
		t.Fatalf("stale version: got %d %s", rr.Code, rr.Body.String())
	}
	rr = transition(r, u.UnitID, "deactivate", "2")
	if rr.Code != 200 || decodeUnit(t, rr).Status != domain.UnitStatusInactive {
		t.Fatalf("deactivate: got %d %s", rr.Code, rr.Body.String())
	}
	// Retired units are never re-activated.
	if rr := transition(r, u.UnitID, "activate", "3"); rr.Code != 409 || errCode(rr) != "invalid_transition" {
		t.Fatalf("activate inactive: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestOrgUnit_IfMatchRequired(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("OPS", "c-1", nil), "p1"))

	if rr := transition(r, u.UnitID, "activate", ""); rr.Code != http.StatusPreconditionRequired {
		t.Fatalf("missing If-Match: want 428, got %d", rr.Code)
	}
	if rr := transition(r, u.UnitID, "activate", "abc"); rr.Code != 400 || errCode(rr) != "invalid_if_match" {
		t.Fatalf("bad If-Match: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := transition(r, "nope", "activate", "1"); rr.Code != 404 {
		t.Fatalf("unknown unit: want 404, got %d", rr.Code)
	}
}

// Spec acceptance: a deactivated parent with an active child is handled.
func TestOrgUnit_CannotDeactivateParentWithActiveChild(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	parent := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("ROOT", "c-1", nil), "p1"))
	transition(r, parent.UnitID, "activate", "1")
	child := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("KID", "c-2", &parent.UnitID), "p1"))

	if rr := transition(r, parent.UnitID, "deactivate", "2"); rr.Code != 409 || errCode(rr) != "has_active_children" {
		t.Fatalf("parent with draft child: got %d %s", rr.Code, rr.Body.String())
	}
	transition(r, child.UnitID, "activate", "1")
	if rr := transition(r, parent.UnitID, "deactivate", "2"); rr.Code != 409 || errCode(rr) != "has_active_children" {
		t.Fatalf("parent with active child: got %d %s", rr.Code, rr.Body.String())
	}
	transition(r, child.UnitID, "deactivate", "2")
	if rr := transition(r, parent.UnitID, "deactivate", "2"); rr.Code != 200 {
		t.Fatalf("parent once children are inactive: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestOrgUnit_ActivateNeedsActiveParent(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	parent := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("ROOT", "c-1", nil), "p1"))
	child := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("KID", "c-2", &parent.UnitID), "p1"))

	if rr := transition(r, child.UnitID, "activate", "1"); rr.Code != 409 || errCode(rr) != "parent_not_active" {
		t.Fatalf("child under draft parent: got %d %s", rr.Code, rr.Body.String())
	}
	transition(r, parent.UnitID, "activate", "1")
	if rr := transition(r, child.UnitID, "activate", "1"); rr.Code != 200 {
		t.Fatalf("child under active parent: got %d %s", rr.Code, rr.Body.String())
	}
}

func TestOrgUnit_AuthzDeniedEverywhere(t *testing.T) {
	denied := &stubAuthZ{err: domain.ErrAuthorizationDenied}
	s := newStubStore()
	// Seed through an allowing router, then use a denying one on the same store.
	allow := newRouter(s, &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	u := decodeUnit(t, doReq(allow, http.MethodPost, "/v1/org/units", unitBody("OPS", "c-1", nil), "p1"))
	r := newRouter(s, &stubPublisher{}, denied, &stubEmployeeValidator{})

	for name, rr := range map[string]*httptest.ResponseRecorder{
		"create":     doReq(r, http.MethodPost, "/v1/org/units", unitBody("NEW", "c-9", nil), "p1"),
		"get":        doReq(r, http.MethodGet, "/v1/org/units/"+u.UnitID, nil, "p1"),
		"list":       doReq(r, http.MethodGet, "/v1/org/units?legal_entity_id=le-us", nil, "p1"),
		"activate":   transition(r, u.UnitID, "activate", "1"),
		"deactivate": transition(r, u.UnitID, "deactivate", "1"),
	} {
		if rr.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", name, rr.Code)
		}
	}
	if s.units[u.UnitID].Status != domain.UnitStatusDraft {
		t.Fatal("a denied command must not change the unit")
	}
}

func TestOrgUnit_ReadAndList(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{}, &stubEmployeeValidator{})
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitBody("OPS", "c-1", nil), "p1"))

	if rr := doReq(r, http.MethodGet, "/v1/org/units/"+u.UnitID+"?as_of=2026-02-01", nil, "p1"); rr.Code != 200 {
		t.Fatalf("get: got %d", rr.Code)
	}
	if rr := doReq(r, http.MethodGet, "/v1/org/units/"+u.UnitID+"?as_of=yesterday", nil, "p1"); rr.Code != 400 || errCode(rr) != "invalid_as_of" {
		t.Fatalf("bad as_of: got %d %s", rr.Code, rr.Body.String())
	}
	if rr := doReq(r, http.MethodGet, "/v1/org/units", nil, "p1"); rr.Code != 400 {
		t.Fatalf("list without legal_entity_id: want 400, got %d", rr.Code)
	}
	rr := doReq(r, http.MethodGet, "/v1/org/units?legal_entity_id=le-none", nil, "p1")
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("empty list must be [], got %d %q", rr.Code, rr.Body.String())
	}
}
