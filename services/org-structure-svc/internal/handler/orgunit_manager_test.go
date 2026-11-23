package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
	"zoiko.io/org-structure-svc/internal/employee"
	"zoiko.io/org-structure-svc/internal/handler"
	"zoiko.io/org-structure-svc/internal/middleware"
)

// ── stub manager operations: same rules as PgStore, in memory ──────────────────

func (s *stubStore) ChangeOrgUnitManager(_ context.Context, id string, req *domain.ChangeManagerRequest, expected int64, _ string) (*domain.OrgUnit, error) {
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
	if u.ManagerEmployeeID != nil {
		if *u.ManagerEmployeeID == req.ManagerEmployeeID {
			return nil, domain.ErrNoManagerChange
		}
		if req.EffectiveAt <= s.managerFrom[id] {
			return nil, domain.ErrEffectiveDateInvalid
		}
	}
	m := req.ManagerEmployeeID
	u.ManagerEmployeeID = &m
	s.managerFrom[id] = req.EffectiveAt
	u.RecordVersion++
	return u, nil
}

func (s *stubStore) FindUnitsByManager(_ context.Context, managerID, _ string) ([]domain.OrgUnit, error) {
	out := []domain.OrgUnit{}
	for _, u := range s.units {
		if u.ManagerEmployeeID != nil && *u.ManagerEmployeeID == managerID && u.Status != domain.UnitStatusInactive {
			out = append(out, *u)
		}
	}
	return out, nil
}

// ── stubs and helpers ──────────────────────────────────────────────────────────

// countingValidator records calls so a test can prove the employee service was
// (or was not) consulted.
type countingValidator struct {
	calls int
	err   error
}

func (v *countingValidator) ValidateEmployee(_ context.Context, _, _, id string) (*employee.Employee, error) {
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	return &employee.Employee{EmployeeID: id, LegalEntityID: "le-us", Status: "ACTIVE"}, nil
}

// entityAuthZ denies the listed legal entities and allows the rest.
type entityAuthZ struct{ deny map[string]bool }

func (a *entityAuthZ) CheckAllowed(_ context.Context, _, entity, _ string) error {
	if a.deny[entity] {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func routerWith(s *stubStore, authz handler.AuthZClient, emp handler.EmployeeValidator) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, authz, emp, zap.NewNop()))
	return r
}

func changeManager(r http.Handler, id, ifMatch string, body map[string]any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/org/units/"+id+"/change-manager", strings.NewReader(string(b)))
	req.Header.Set("X-Principal-Id", "p1")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func mgrMove(manager, at string) map[string]any {
	return map[string]any{"manager_employee_id": manager, "effective_at": at, "reason": "handover"}
}

func unitIn(entity, code, corr string, manager *string) map[string]any {
	b := unitBody(code, corr, nil)
	b["legal_entity_id"] = entity
	if manager != nil {
		b["manager_employee_id"] = *manager
	}
	return b
}

// ── ChangeManager ──────────────────────────────────────────────────────────────

func TestChangeManager_NamesNewManagerAndBumpsVersion(t *testing.T) {
	emp := &countingValidator{}
	r := routerWith(newStubStore(), &stubAuthZ{}, emp)
	old := "emp-old"
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", &old), "p1"))

	rr := changeManager(r, u.UnitID, "1", mgrMove("emp-new", "2026-03-01"))
	if rr.Code != 200 {
		t.Fatalf("change manager: got %d %s", rr.Code, rr.Body.String())
	}
	got := decodeUnit(t, rr)
	if got.ManagerEmployeeID == nil || *got.ManagerEmployeeID != "emp-new" || got.RecordVersion != 2 {
		t.Fatalf("want emp-new at v2, got %v v%d", got.ManagerEmployeeID, got.RecordVersion)
	}
}

func TestChangeManager_UnknownManagerRejectedAndNothingWritten(t *testing.T) {
	s := newStubStore()
	good := routerWith(s, &stubAuthZ{}, &countingValidator{})
	u := decodeUnit(t, doReq(good, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", nil), "p1"))

	bad := routerWith(s, &stubAuthZ{}, &countingValidator{err: domain.ErrEmployeeNotFound})
	rr := changeManager(bad, u.UnitID, "1", mgrMove("ghost", "2026-03-01"))
	if rr.Code != 400 || errCode(rr) != "manager_invalid" {
		t.Fatalf("unknown manager: got %d %s", rr.Code, rr.Body.String())
	}
	if s.units[u.UnitID].ManagerEmployeeID != nil || s.units[u.UnitID].RecordVersion != 1 {
		t.Fatal("a rejected manager must change nothing")
	}
}

// Fails closed: if employee-master-svc is unreachable, no manager is written.
func TestChangeManager_EmployeeServiceDownFailsClosed(t *testing.T) {
	s := newStubStore()
	good := routerWith(s, &stubAuthZ{}, &countingValidator{})
	u := decodeUnit(t, doReq(good, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", nil), "p1"))

	down := routerWith(s, &stubAuthZ{}, &countingValidator{err: errors.New("connection refused")})
	rr := changeManager(down, u.UnitID, "1", mgrMove("emp-1", "2026-03-01"))
	if rr.Code != 503 || errCode(rr) != "employee_validation_failed" {
		t.Fatalf("service down: got %d %s", rr.Code, rr.Body.String())
	}
	if s.units[u.UnitID].ManagerEmployeeID != nil {
		t.Fatal("no manager may be written when the employee cannot be verified")
	}

	missing := routerWith(s, &stubAuthZ{}, nil) // no validator configured at all
	if rr := changeManager(missing, u.UnitID, "1", mgrMove("emp-1", "2026-03-01")); rr.Code != 503 {
		t.Fatalf("no validator configured: want 503, got %d", rr.Code)
	}
}

func TestChangeManager_Validation(t *testing.T) {
	r := routerWith(newStubStore(), &stubAuthZ{}, &countingValidator{})
	m := "emp-1"
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", &m), "p1"))

	cases := []struct {
		name    string
		unit    string
		ifMatch string
		body    map[string]any
		code    int
		err     string
	}{
		{"no If-Match", u.UnitID, "", mgrMove("emp-2", "2026-03-01"), 428, "if_match_required"},
		{"stale version", u.UnitID, "4", mgrMove("emp-2", "2026-03-01"), 409, "version_conflict"},
		{"same manager", u.UnitID, "1", mgrMove("emp-1", "2026-03-01"), 409, "no_change"},
		{"effective on the manager's start", u.UnitID, "1", mgrMove("emp-2", "2026-01-01"), 422, "invalid_effective_date"},
		{"bad date", u.UnitID, "1", mgrMove("emp-2", "soon"), 400, "invalid_effective_at"},
		{"no manager id", u.UnitID, "1", mgrMove("", "2026-03-01"), 400, "missing_fields"},
		{"no reason", u.UnitID, "1", map[string]any{"manager_employee_id": "emp-2", "effective_at": "2026-03-01"}, 400, "missing_fields"},
		{"unknown unit", "nope", "1", mgrMove("emp-2", "2026-03-01"), 404, "org_unit_not_found"},
	}
	for _, c := range cases {
		rr := changeManager(r, c.unit, c.ifMatch, c.body)
		if rr.Code != c.code || errCode(rr) != c.err {
			t.Errorf("%s: want %d %s, got %d %s", c.name, c.code, c.err, rr.Code, rr.Body.String())
		}
	}
}

// A caller without rights must not be able to probe which employee ids exist.
func TestChangeManager_DeniedCallerNeverReachesEmployeeService(t *testing.T) {
	s := newStubStore()
	allow := routerWith(s, &stubAuthZ{}, &countingValidator{})
	u := decodeUnit(t, doReq(allow, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", nil), "p1"))

	emp := &countingValidator{}
	deny := routerWith(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, emp)
	if rr := changeManager(deny, u.UnitID, "1", mgrMove("emp-1", "2026-03-01")); rr.Code != 403 {
		t.Fatalf("denied: want 403, got %d", rr.Code)
	}
	if emp.calls != 0 {
		t.Fatalf("employee service was called %d times for a denied caller", emp.calls)
	}
}

func TestChangeManager_InactiveUnitRefused(t *testing.T) {
	r := routerWith(newStubStore(), &stubAuthZ{}, &countingValidator{})
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", nil), "p1"))
	transition(r, u.UnitID, "activate", "1")
	transition(r, u.UnitID, "deactivate", "2")

	if rr := changeManager(r, u.UnitID, "3", mgrMove("emp-1", "2026-03-01")); rr.Code != 409 || errCode(rr) != "invalid_transition" {
		t.Fatalf("inactive unit: got %d %s", rr.Code, rr.Body.String())
	}
}

// Create now resolves the manager too (it used to take the id on trust).
func TestCreateOrgUnit_ManagerIsResolved(t *testing.T) {
	m := "emp-1"
	bad := routerWith(newStubStore(), &stubAuthZ{}, &countingValidator{err: domain.ErrEmployeeNotFound})
	if rr := doReq(bad, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", &m), "p1"); rr.Code != 400 || errCode(rr) != "manager_invalid" {
		t.Fatalf("unknown manager on create: got %d %s", rr.Code, rr.Body.String())
	}

	s := newStubStore()
	good := routerWith(s, &stubAuthZ{}, &countingValidator{})
	rr := doReq(good, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-2", &m), "p1")
	if rr.Code != 201 || decodeUnit(t, rr).ManagerEmployeeID == nil {
		t.Fatalf("valid manager on create: got %d %s", rr.Code, rr.Body.String())
	}
	if len(s.units) != 1 {
		t.Fatalf("exactly the valid create should be stored, got %d units", len(s.units))
	}
}

// ── FindUnitsByManager ─────────────────────────────────────────────────────────

func TestFindUnitsByManager(t *testing.T) {
	s := newStubStore()
	all := routerWith(s, &stubAuthZ{}, &countingValidator{})
	m := "emp-1"
	doReq(all, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", &m), "p1")
	doReq(all, http.MethodPost, "/v1/org/units", unitIn("le-eu", "OPS", "c-2", &m), "p1")
	doReq(all, http.MethodPost, "/v1/org/units", unitIn("le-us", "HR", "c-3", nil), "p1")
	other := "emp-2"
	doReq(all, http.MethodPost, "/v1/org/units", unitIn("le-us", "LEGAL", "c-4", &other), "p1")

	list := func(r http.Handler, q string) (int, []domain.OrgUnit) {
		rr := doReq(r.(chi.Router), http.MethodGet, "/v1/org/units?"+q, nil, "p1")
		var out []domain.OrgUnit
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	if code, out := list(all, "manager_employee_id=emp-1"); code != 200 || len(out) != 2 {
		t.Fatalf("both entities: got %d with %d units", code, len(out))
	}
	if code, out := list(all, "manager_employee_id=emp-1&legal_entity_id=le-eu"); code != 200 || len(out) != 1 || out[0].Code != "OPS" {
		t.Fatalf("filtered to one entity: got %d %+v", code, out)
	}
	if code, out := list(all, "manager_employee_id=nobody"); code != 200 || len(out) != 0 {
		t.Fatalf("a manager of nothing gets an empty list, got %d %d", code, len(out))
	}

	// Entities the caller may not view are left out ...
	partial := routerWith(s, &entityAuthZ{deny: map[string]bool{"le-eu": true}}, &countingValidator{})
	if code, out := list(partial, "manager_employee_id=emp-1"); code != 200 || len(out) != 1 || out[0].LegalEntityID != "le-us" {
		t.Fatalf("partial access: got %d %+v", code, out)
	}
	// ... and a caller who may view none gets 403, not an empty list.
	none := routerWith(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, &countingValidator{})
	if code, _ := list(none, "manager_employee_id=emp-1"); code != 403 {
		t.Fatalf("no access: want 403, got %d", code)
	}
}

func TestFindUnitsByManager_SkipsInactiveUnits(t *testing.T) {
	r := routerWith(newStubStore(), &stubAuthZ{}, &countingValidator{})
	m := "emp-1"
	u := decodeUnit(t, doReq(r, http.MethodPost, "/v1/org/units", unitIn("le-us", "FIN", "c-1", &m), "p1"))
	transition(r, u.UnitID, "activate", "1")
	transition(r, u.UnitID, "deactivate", "2")

	rr := doReq(r, http.MethodGet, "/v1/org/units?manager_employee_id=emp-1", nil, "p1")
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("retired units have nothing to manage: got %d %q", rr.Code, rr.Body.String())
	}
}
