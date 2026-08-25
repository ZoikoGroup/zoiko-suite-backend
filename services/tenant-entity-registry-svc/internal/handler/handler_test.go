// Tests for the HTTP layer of tenant-entity-registry-svc.
//
// This package previously had no test of any kind: handler.New took the
// concrete *registry.Service, so reaching a handler meant standing up a real
// service over a real store, and all 27 routes went unexercised. The interface
// is now injectable, and what these tests pin is specifically what the HTTP
// layer decides rather than what the service decides:
//
//   - the route table — that every declared method+path reaches its handler at
//     all, which is what catches a route registered on the wrong verb;
//   - the success status of each route (201 create / 200 read / 204 command),
//     since a caller distinguishing "created" from "accepted" depends on it;
//   - the sentinel-to-status mapping in writeErr, which is the whole contract
//     between the service's error vocabulary and the API;
//   - the URL parameter each handler extracts, which is the one thing a copied
//     handler body gets silently wrong — reading {entityID} where the route
//     declares {tenantID} yields an empty id, not an error;
//   - end_date parsing on the two end-dating routes, and correlation-id
//     propagation.
package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/handler"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	tenantID   = "11111111-1111-1111-1111-111111111111"
	entityID   = "22222222-2222-2222-2222-222222222222"
	wsID       = "33333333-3333-3333-3333-333333333333"
	hierID     = "44444444-4444-4444-4444-444444444444"
	assignID   = "55555555-5555-5555-5555-555555555555"
	policyID   = "66666666-6666-6666-6666-666666666666"
	regionID   = "77777777-7777-7777-7777-777777777777"
	bundleID   = "88888888-8888-8888-8888-888888888888"
	corrID     = "corr-abc-123"
	endDateRaw = "2026-09-01T00:00:00Z"
)

// ── Stub service ─────────────────────────────────────────────────────────────

// stubSvc implements handler.Service. err is returned by every method, so one
// stub drives the whole error-mapping table; gotID records the identifier the
// handler pulled out of the URL, which is how the parameter-plumbing tests
// detect a handler reading the wrong chi parameter.
type stubSvc struct {
	err error

	gotID      string
	gotCorrID  string
	gotEndDate time.Time
	gotStatus  domain.TransitionEntityStatusRequest
}

func (s *stubSvc) ProvisionTenant(_ context.Context, _ domain.ProvisionTenantRequest, correlationID string) (*domain.Tenant, error) {
	s.gotCorrID = correlationID
	return &domain.Tenant{TenantID: tenantID}, s.err
}

func (s *stubSvc) GetTenant(_ context.Context, id string) (*domain.Tenant, error) {
	s.gotID = id
	return &domain.Tenant{TenantID: id}, s.err
}

func (s *stubSvc) TransitionTenantLifecycle(_ context.Context, id string, _ domain.TransitionTenantLifecycleRequest) error {
	s.gotID = id
	return s.err
}

func (s *stubSvc) CreateEntity(_ context.Context, _ domain.CreateEntityRequest) (*domain.LegalEntity, error) {
	return &domain.LegalEntity{LegalEntityID: entityID}, s.err
}

func (s *stubSvc) GetEntity(_ context.Context, id string) (*domain.LegalEntity, error) {
	s.gotID = id
	return &domain.LegalEntity{LegalEntityID: id}, s.err
}

func (s *stubSvc) ListEntities(_ context.Context, id string) ([]*domain.LegalEntity, error) {
	s.gotID = id
	return []*domain.LegalEntity{{LegalEntityID: entityID}}, s.err
}

func (s *stubSvc) CreateWorkspace(_ context.Context, _ domain.CreateWorkspaceRequest) (*domain.Workspace, error) {
	return &domain.Workspace{WorkspaceID: wsID}, s.err
}

func (s *stubSvc) GetWorkspace(_ context.Context, id string) (*domain.Workspace, error) {
	s.gotID = id
	return &domain.Workspace{WorkspaceID: id}, s.err
}

func (s *stubSvc) ListWorkspaces(_ context.Context, id string) ([]*domain.Workspace, error) {
	s.gotID = id
	return []*domain.Workspace{{WorkspaceID: wsID}}, s.err
}

func (s *stubSvc) UpdateWorkspace(_ context.Context, id string, req domain.UpdateWorkspaceRequest) (*domain.Workspace, error) {
	s.gotID = id
	s.gotCorrID = req.CorrelationID
	return &domain.Workspace{WorkspaceID: id}, s.err
}

func (s *stubSvc) TransitionWorkspaceStatus(_ context.Context, id string, req domain.TransitionWorkspaceStatusRequest) error {
	s.gotID = id
	s.gotCorrID = req.CorrelationID
	return s.err
}

func (s *stubSvc) UpdateEntity(_ context.Context, id string, _ domain.UpdateEntityRequest) (*domain.LegalEntity, error) {
	s.gotID = id
	return &domain.LegalEntity{LegalEntityID: id}, s.err
}

func (s *stubSvc) GetEntityStatus(_ context.Context, id string) (*domain.EntityStatusResponse, error) {
	s.gotID = id
	return &domain.EntityStatusResponse{EntityID: id}, s.err
}

func (s *stubSvc) TransitionEntityStatus(_ context.Context, id string, req domain.TransitionEntityStatusRequest) error {
	s.gotID = id
	s.gotStatus = req
	return s.err
}

func (s *stubSvc) CreateHierarchy(_ context.Context, _ domain.CreateHierarchyRequest) (*domain.EntityHierarchy, error) {
	return &domain.EntityHierarchy{HierarchyID: hierID}, s.err
}

func (s *stubSvc) EndDateHierarchy(_ context.Context, id string, endDate time.Time, correlationID string) error {
	s.gotID, s.gotEndDate, s.gotCorrID = id, endDate, correlationID
	return s.err
}

func (s *stubSvc) ListHierarchies(_ context.Context, id string) ([]*domain.EntityHierarchy, error) {
	s.gotID = id
	return []*domain.EntityHierarchy{{HierarchyID: hierID}}, s.err
}

func (s *stubSvc) AssignJurisdiction(_ context.Context, id string, _ domain.AssignJurisdictionRequest) (*domain.EntityJurisdictionAssignment, error) {
	s.gotID = id
	return &domain.EntityJurisdictionAssignment{AssignmentID: assignID}, s.err
}

func (s *stubSvc) ListJurisdictions(_ context.Context, id string) ([]*domain.EntityJurisdictionAssignment, error) {
	s.gotID = id
	return []*domain.EntityJurisdictionAssignment{{AssignmentID: assignID}}, s.err
}

func (s *stubSvc) EndDateJurisdictionAssignment(_ context.Context, id string, endDate time.Time, correlationID string) error {
	s.gotID, s.gotEndDate, s.gotCorrID = id, endDate, correlationID
	return s.err
}

func (s *stubSvc) CreateResidencyPolicy(_ context.Context, _ domain.CreateResidencyPolicyRequest) (*domain.DataResidencyPolicy, error) {
	return &domain.DataResidencyPolicy{DataResidencyPolicyID: policyID}, s.err
}

func (s *stubSvc) GetResidencyPolicy(_ context.Context, id string) (*domain.DataResidencyPolicy, error) {
	s.gotID = id
	return &domain.DataResidencyPolicy{DataResidencyPolicyID: id}, s.err
}

func (s *stubSvc) GetResidencyRegion(_ context.Context, id string) (*domain.ResidencyRegion, error) {
	s.gotID = id
	return &domain.ResidencyRegion{ResidencyRegionID: id}, s.err
}

func (s *stubSvc) ListResidencyRegions(_ context.Context) ([]*domain.ResidencyRegion, error) {
	return []*domain.ResidencyRegion{{ResidencyRegionID: regionID}}, s.err
}

func (s *stubSvc) ResolveTenantRegion(_ context.Context, id string) (*domain.ResolvedTenantRegion, error) {
	s.gotID = id
	return &domain.ResolvedTenantRegion{TenantID: id, RegionCode: "eu-west"}, s.err
}

func (s *stubSvc) CreateTaxIdentityBundle(_ context.Context, id string, _ domain.CreateTaxIdentityBundleRequest) (*domain.TaxIdentityBundle, error) {
	s.gotID = id
	return &domain.TaxIdentityBundle{TaxIdentityBundleID: bundleID}, s.err
}

func (s *stubSvc) GetTaxIdentityBundle(_ context.Context, id string) (*domain.TaxIdentityBundle, error) {
	s.gotID = id
	return &domain.TaxIdentityBundle{TaxIdentityBundleID: id}, s.err
}

func (s *stubSvc) ListTaxIdentityBundles(_ context.Context, id string) ([]*domain.TaxIdentityBundle, error) {
	s.gotID = id
	return []*domain.TaxIdentityBundle{{TaxIdentityBundleID: bundleID}}, s.err
}

func (s *stubSvc) TransitionTaxIdentityBundleStatus(_ context.Context, id string, _ domain.TransitionTaxIdentityBundleStatusRequest) error {
	s.gotID = id
	return s.err
}

// Compile-time proof the stub still satisfies the interface the handler takes.
// If a route gains a service method, this fails here rather than in 30 tests.
var _ handler.Service = (*stubSvc)(nil)

// ── Harness ──────────────────────────────────────────────────────────────────

func newRouter(s *stubSvc) chi.Router {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, zap.NewNop()))
	return r
}

func do(r chi.Router, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", corrID)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// route is one entry in the API surface: what a caller sends and what they
// must get back when the service succeeds.
type route struct {
	name    string
	method  string
	path    string
	body    string
	wantSts int
	// wantID is the identifier the handler must have extracted from the path.
	// Empty means the route carries no identifier to check.
	wantID string
}

func allRoutes() []route {
	return []route{
		{"provision tenant", http.MethodPost, "/v1/tenants", `{"tenant_code":"T1"}`, http.StatusCreated, ""},
		{"get tenant", http.MethodGet, "/v1/tenants/" + tenantID, "", http.StatusOK, tenantID},
		{"tenant lifecycle", http.MethodPost, "/v1/tenants/" + tenantID + "/lifecycle", `{"new_lifecycle_state":"ACTIVE"}`, http.StatusNoContent, tenantID},
		{"resolve tenant region", http.MethodGet, "/v1/tenants/" + tenantID + "/residency-region", "", http.StatusOK, tenantID},
		{"list entities", http.MethodGet, "/v1/tenants/" + tenantID + "/entities", "", http.StatusOK, tenantID},
		{"list workspaces", http.MethodGet, "/v1/tenants/" + tenantID + "/workspaces", "", http.StatusOK, tenantID},
		{"create workspace", http.MethodPost, "/v1/workspaces", `{"name":"W1"}`, http.StatusCreated, ""},
		{"get workspace", http.MethodGet, "/v1/workspaces/" + wsID, "", http.StatusOK, wsID},
		{"update workspace", http.MethodPatch, "/v1/workspaces/" + wsID, `{"name":"W2"}`, http.StatusOK, wsID},
		{"workspace status", http.MethodPost, "/v1/workspaces/" + wsID + "/status", `{"new_status":"ARCHIVED"}`, http.StatusNoContent, wsID},
		{"create entity", http.MethodPost, "/v1/entities", `{"entity_code":"E1"}`, http.StatusCreated, ""},
		{"get entity", http.MethodGet, "/v1/entities/" + entityID, "", http.StatusOK, entityID},
		{"update entity", http.MethodPatch, "/v1/entities/" + entityID, `{"legal_name":"New"}`, http.StatusOK, entityID},
		{"get entity status", http.MethodGet, "/v1/entities/" + entityID + "/status", "", http.StatusOK, entityID},
		{"transition entity status", http.MethodPost, "/v1/entities/" + entityID + "/status", `{"new_status":"ACTIVE"}`, http.StatusNoContent, entityID},
		{"create hierarchy", http.MethodPost, "/v1/entity-hierarchies", `{"parent_legal_entity_id":"p"}`, http.StatusCreated, ""},
		{"list hierarchies", http.MethodGet, "/v1/entities/" + entityID + "/hierarchies", "", http.StatusOK, entityID},
		{"end-date hierarchy", http.MethodDelete, "/v1/entity-hierarchies/" + hierID + "?end_date=" + endDateRaw, "", http.StatusNoContent, hierID},
		{"assign jurisdiction", http.MethodPost, "/v1/entities/" + entityID + "/jurisdictions", `{"jurisdiction_id":"j"}`, http.StatusCreated, entityID},
		{"list jurisdictions", http.MethodGet, "/v1/entities/" + entityID + "/jurisdictions", "", http.StatusOK, entityID},
		{"end-date jurisdiction", http.MethodDelete, "/v1/entity-jurisdictions/" + assignID + "?end_date=" + endDateRaw, "", http.StatusNoContent, assignID},
		{"create residency policy", http.MethodPost, "/v1/residency-policies", `{"policy_code":"P1"}`, http.StatusCreated, ""},
		{"get residency policy", http.MethodGet, "/v1/residency-policies/" + policyID, "", http.StatusOK, policyID},
		{"list residency regions", http.MethodGet, "/v1/residency-regions", "", http.StatusOK, ""},
		{"get residency region", http.MethodGet, "/v1/residency-regions/" + regionID, "", http.StatusOK, regionID},
		{"create tax bundle", http.MethodPost, "/v1/entities/" + entityID + "/tax-identity-bundles", `{"jurisdiction_id":"j"}`, http.StatusCreated, entityID},
		{"list tax bundles", http.MethodGet, "/v1/entities/" + entityID + "/tax-identity-bundles", "", http.StatusOK, entityID},
		{"get tax bundle", http.MethodGet, "/v1/tax-identity-bundles/" + bundleID, "", http.StatusOK, bundleID},
		{"tax bundle status", http.MethodPost, "/v1/tax-identity-bundles/" + bundleID + "/status", `{"new_status":"ACTIVE"}`, http.StatusNoContent, bundleID},
	}
}

// ── The route table ──────────────────────────────────────────────────────────

// Every declared route reaches its handler and returns its documented success
// status. A route registered on the wrong verb fails here as a 405.
func TestRoutes_SuccessStatus(t *testing.T) {
	for _, rt := range allRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			stub := &stubSvc{}
			rec := do(newRouter(stub), rt.method, rt.path, rt.body)
			if rec.Code != rt.wantSts {
				t.Fatalf("%s %s = %d, want %d (body %s)", rt.method, rt.path, rec.Code, rt.wantSts, rec.Body.String())
			}
		})
	}
}

// The identifier each handler extracts must be the one its route declares.
// Reading the wrong chi parameter yields "" rather than an error, so without
// this a handler can look correct and operate on nothing.
func TestRoutes_ExtractDeclaredURLParam(t *testing.T) {
	for _, rt := range allRoutes() {
		if rt.wantID == "" {
			continue
		}
		t.Run(rt.name, func(t *testing.T) {
			stub := &stubSvc{}
			do(newRouter(stub), rt.method, rt.path, rt.body)
			if stub.gotID != rt.wantID {
				t.Fatalf("%s %s passed id %q, want %q", rt.method, rt.path, stub.gotID, rt.wantID)
			}
		})
	}
}

// 204 means "done, nothing to say". A body on a 204 is a protocol violation
// some proxies reject outright.
func TestRoutes_NoContentRepliesHaveEmptyBody(t *testing.T) {
	for _, rt := range allRoutes() {
		if rt.wantSts != http.StatusNoContent {
			continue
		}
		t.Run(rt.name, func(t *testing.T) {
			rec := do(newRouter(&stubSvc{}), rt.method, rt.path, rt.body)
			if rec.Body.Len() != 0 {
				t.Fatalf("204 carried a body: %q", rec.Body.String())
			}
		})
	}
}

// ── Error mapping ────────────────────────────────────────────────────────────

// writeErr is the contract between the service's error vocabulary and the API.
// Every sentinel is pinned, including the two that share a status for different
// reasons (ErrConflict and ErrRegionUnresolved both 409) and the pair that a
// reader most often collapses: unauthenticated is 401, unauthorized is 403.
func TestWriteErr_SentinelToStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"not found", registry.ErrNotFound, http.StatusNotFound},
		{"invalid transition", registry.ErrInvalidTransition, http.StatusUnprocessableEntity},
		{"unauthenticated", registry.ErrUnauthenticated, http.StatusUnauthorized},
		{"unauthorized", registry.ErrUnauthorized, http.StatusForbidden},
		{"upstream unavailable", registry.ErrServiceUnavailable, http.StatusServiceUnavailable},
		{"invalid input", registry.ErrInvalidInput, http.StatusBadRequest},
		{"conflict", registry.ErrConflict, http.StatusConflict},
		{"region unresolved", registry.ErrRegionUnresolved, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(newRouter(&stubSvc{err: tc.err}), http.MethodGet, "/v1/tenants/"+tenantID, "")
			if rec.Code != tc.want {
				t.Fatalf("%v mapped to %d, want %d", tc.err, rec.Code, tc.want)
			}
		})
	}
}

// An unrecognised error must be a 500 with a generic message — never the
// underlying error text, which can name internal hosts, tables or SQL.
func TestWriteErr_UnknownErrorIs500AndDoesNotLeak(t *testing.T) {
	secret := errString(`pq: relation "internal_secrets" does not exist`)
	rec := do(newRouter(&stubSvc{err: secret}), http.MethodGet, "/v1/tenants/"+tenantID, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for an unmapped error, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "internal_secrets") {
		t.Fatalf("500 leaked the underlying error: %s", rec.Body.String())
	}
}

// Every sentinel mapping must apply on every route, not only the one the table
// above happens to use — writeErr is shared, and this proves each handler
// routes its error through it rather than writing its own status.
func TestWriteErr_AppliesOnEveryRoute(t *testing.T) {
	for _, rt := range allRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			rec := do(newRouter(&stubSvc{err: registry.ErrNotFound}), rt.method, rt.path, rt.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s with ErrNotFound = %d, want 404", rt.method, rt.path, rec.Code)
			}
		})
	}
}

// The correlation id is the thread an operator follows across services, so a
// refusal that drops it is a refusal nobody can trace back.
func TestWriteErr_EchoesCorrelationID(t *testing.T) {
	rec := do(newRouter(&stubSvc{err: registry.ErrNotFound}), http.MethodGet, "/v1/tenants/"+tenantID, "")

	var body struct {
		Error         string `json:"error"`
		CorrelationID string `json:"correlation_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error response is not JSON: %v", err)
	}
	if body.CorrelationID != corrID {
		t.Fatalf("correlation_id = %q, want %q", body.CorrelationID, corrID)
	}
}

// ── Request body decoding ────────────────────────────────────────────────────

func TestDecode_MalformedJSONIs400(t *testing.T) {
	for _, rt := range allRoutes() {
		if rt.body == "" {
			continue
		}
		t.Run(rt.name, func(t *testing.T) {
			rec := do(newRouter(&stubSvc{}), rt.method, rt.path, `{"broken":`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s %s with malformed JSON = %d, want 400", rt.method, rt.path, rec.Code)
			}
		})
	}
}

// ── end_date on the end-dating routes ────────────────────────────────────────

// Both end-dating routes take end_date as a query parameter. Omitting it must
// be refused rather than defaulting: end-dating a relationship "now" because a
// caller forgot the parameter silently closes a record they meant to keep.
func TestEndDate_RequiredAndRFC3339(t *testing.T) {
	paths := map[string]string{
		"hierarchy":    "/v1/entity-hierarchies/" + hierID,
		"jurisdiction": "/v1/entity-jurisdictions/" + assignID,
	}
	for name, base := range paths {
		t.Run(name+"/missing", func(t *testing.T) {
			rec := do(newRouter(&stubSvc{}), http.MethodDelete, base, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("missing end_date = %d, want 400", rec.Code)
			}
		})
		t.Run(name+"/malformed", func(t *testing.T) {
			rec := do(newRouter(&stubSvc{}), http.MethodDelete, base+"?end_date=2026-09-01", "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("bare calendar date = %d, want 400 (RFC3339 required)", rec.Code)
			}
		})
		t.Run(name+"/parsed", func(t *testing.T) {
			stub := &stubSvc{}
			rec := do(newRouter(stub), http.MethodDelete, base+"?end_date="+endDateRaw, "")
			if rec.Code != http.StatusNoContent {
				t.Fatalf("valid end_date = %d, want 204 (%s)", rec.Code, rec.Body.String())
			}
			want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			if !stub.gotEndDate.Equal(want) {
				t.Fatalf("end_date passed as %s, want %s", stub.gotEndDate, want)
			}
			if stub.gotCorrID != corrID {
				t.Fatalf("correlation id passed as %q, want %q", stub.gotCorrID, corrID)
			}
		})
	}
}

// ── Correlation id into the service ──────────────────────────────────────────

// ProvisionTenant and TransitionEntityStatus are the two handlers that pass the
// correlation id inward rather than only echoing it on failure. Provisioning
// takes it as an argument; the status transition overwrites whatever the body
// claimed, so a caller cannot forge the audit thread of their own request.
func TestCorrelationID_ReachesTheService(t *testing.T) {
	t.Run("provision tenant", func(t *testing.T) {
		stub := &stubSvc{}
		do(newRouter(stub), http.MethodPost, "/v1/tenants", `{"tenant_code":"T1"}`)
		if stub.gotCorrID != corrID {
			t.Fatalf("correlation id = %q, want %q", stub.gotCorrID, corrID)
		}
	})

	t.Run("workspace patch overrides the body", func(t *testing.T) {
		stub := &stubSvc{}
		do(newRouter(stub), http.MethodPatch, "/v1/workspaces/"+wsID,
			`{"name":"W2","correlation_id":"forged-by-caller"}`)
		if stub.gotCorrID != corrID {
			t.Fatalf("correlation id = %q, want the header value %q", stub.gotCorrID, corrID)
		}
	})

	t.Run("workspace status overrides the body", func(t *testing.T) {
		stub := &stubSvc{}
		do(newRouter(stub), http.MethodPost, "/v1/workspaces/"+wsID+"/status",
			`{"new_status":"ARCHIVED","correlation_id":"forged-by-caller"}`)
		if stub.gotCorrID != corrID {
			t.Fatalf("correlation id = %q, want the header value %q", stub.gotCorrID, corrID)
		}
	})

	t.Run("entity status overrides the body", func(t *testing.T) {
		stub := &stubSvc{}
		do(newRouter(stub), http.MethodPost, "/v1/entities/"+entityID+"/status",
			`{"new_status":"ACTIVE","correlation_id":"forged-by-caller"}`)
		if stub.gotStatus.CorrelationID != corrID {
			t.Fatalf("correlation id = %q, want the header value %q — a caller must not set it",
				stub.gotStatus.CorrelationID, corrID)
		}
	})
}

// An absent X-Correlation-ID is tolerated rather than fatal — the header is
// propagated evidence, not an input the API requires.
func TestCorrelationID_AbsentIsTolerated(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+tenantID, nil)
	rec := httptest.NewRecorder()
	newRouter(&stubSvc{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("no correlation header = %d, want 200", rec.Code)
	}
}

// errString is a minimal error type for the unmapped-error case, so the test
// can assert on text that must not reach the client.
type errString string

func (e errString) Error() string { return string(e) }

// The route table above is only a completeness claim if something checks it
// against the router. This walks every route RegisterRoutes actually mounts and
// fails if one is missing from allRoutes(), so a route added without a test
// fails here rather than shipping unexercised — which is how all 27 of these
// came to have no test in the first place.
func TestRouteTable_CoversEveryRegisteredRoute(t *testing.T) {
	mux, ok := newRouter(&stubSvc{}).(*chi.Mux)
	if !ok {
		t.Fatal("router is not a *chi.Mux")
	}

	covered := map[string]bool{}
	for _, rt := range allRoutes() {
		path := rt.path
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		rctx := chi.NewRouteContext()
		if !mux.Match(rctx, rt.method, path) {
			t.Fatalf("table entry %s %s matches no registered route", rt.method, path)
		}
		covered[rt.method+" "+rctx.RoutePattern()] = true
	}

	var missing []string
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		norm := route
		if len(norm) > 1 {
			norm = strings.TrimSuffix(norm, "/")
		}
		if !covered[method+" "+norm] {
			missing = append(missing, method+" "+norm)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("registered routes with no entry in allRoutes(): %s", strings.Join(missing, ", "))
	}
}
