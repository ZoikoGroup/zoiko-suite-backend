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
	"fmt"
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

	// ORG-02/ORG-03 recordings — see org_stub_test.go.
	gotCommand        domain.TenantCommand
	gotHostname       string
	gotClaimedTenant  string
	gotAsOf           time.Time
	gotRegistryNumber string
	gotJurisdiction   string
	gotOpenOnly       bool
	// hostTenantErr drives VerifyHostTenant only, so the §8 NP3 refusal can be
	// exercised without every other handler test failing at the host check.
	hostTenantErr error
}

func (s *stubSvc) ProvisionTenant(_ context.Context, _ domain.ProvisionTenantRequest, correlationID string) (*domain.Tenant, error) {
	s.gotCorrID = correlationID
	return &domain.Tenant{TenantID: tenantID}, s.err
}

func (s *stubSvc) GetTenant(_ context.Context, id string) (*domain.Tenant, error) {
	s.gotID = id
	return &domain.Tenant{TenantID: id, RecordVersion: 1}, s.err
}

func (s *stubSvc) TransitionTenantLifecycle(_ context.Context, id string, _ domain.TransitionTenantLifecycleRequest) error {
	s.gotID = id
	return s.err
}

func (s *stubSvc) CreateEntity(_ context.Context, req domain.CreateEntityRequest) (*domain.LegalEntity, error) {
	s.gotCorrID = req.CorrelationID
	return &domain.LegalEntity{LegalEntityID: entityID}, s.err
}

func (s *stubSvc) GetEntity(_ context.Context, id string) (*domain.LegalEntity, error) {
	s.gotID = id
	return &domain.LegalEntity{LegalEntityID: id, RecordVersion: 1}, s.err
}

func (s *stubSvc) ListEntities(_ context.Context, id string) ([]*domain.LegalEntity, error) {
	s.gotID = id
	return []*domain.LegalEntity{{LegalEntityID: entityID}}, s.err
}

func (s *stubSvc) CreateWorkspace(_ context.Context, req domain.CreateWorkspaceRequest) (*domain.Workspace, error) {
	s.gotCorrID = req.CorrelationID
	return &domain.Workspace{WorkspaceID: wsID}, s.err
}

func (s *stubSvc) GetWorkspace(_ context.Context, id string) (*domain.Workspace, error) {
	s.gotID = id
	return &domain.Workspace{WorkspaceID: id, RecordVersion: 1}, s.err
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

func (s *stubSvc) UpdateEntity(_ context.Context, id string, req domain.UpdateEntityRequest) (*domain.LegalEntity, error) {
	s.gotCorrID = req.CorrelationID
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

func (s *stubSvc) CreateHierarchy(_ context.Context, req domain.CreateHierarchyRequest) (*domain.EntityHierarchy, error) {
	s.gotCorrID = req.CorrelationID
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

func (s *stubSvc) AssignJurisdiction(_ context.Context, id string, req domain.AssignJurisdictionRequest) (*domain.EntityJurisdictionAssignment, error) {
	s.gotCorrID = req.CorrelationID
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
	return &domain.DataResidencyPolicy{DataResidencyPolicyID: id, RecordVersion: 1}, s.err
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
	return &domain.TaxIdentityBundle{TaxIdentityBundleID: id, RecordVersion: 1}, s.err
}

func (s *stubSvc) GetHierarchyByID(_ context.Context, id string) (*domain.EntityHierarchy, error) {
	s.gotID = id
	return &domain.EntityHierarchy{HierarchyID: id, RecordVersion: 1}, s.err
}

func (s *stubSvc) GetJurisdictionAssignmentByID(_ context.Context, id string) (*domain.EntityJurisdictionAssignment, error) {
	s.gotID = id
	return &domain.EntityJurisdictionAssignment{AssignmentID: id, RecordVersion: 1}, s.err
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
	// Add If-Match header for mutating requests (required by API-3)
	if method == http.MethodPatch || method == http.MethodPost || method == http.MethodDelete {
		// Check if this is a mutation of an existing aggregate (not a create)
		// Routes that mutate existing aggregates:
		// - /v1/tenants/{id}/lifecycle
		// - /v1/workspaces/{id} (PATCH)
		// - /v1/workspaces/{id}/status
		// - /v1/entities/{id} (PATCH)
		// - /v1/entities/{id}/status
		// - /v1/entity-hierarchies/{id} (DELETE)
		// - /v1/entity-jurisdictions/{id} (DELETE)
		// - /v1/tax-identity-bundles/{id}/status
		if strings.Contains(path, "/lifecycle") ||
			strings.Contains(path, "/status") ||
			(method == http.MethodPatch && (strings.Contains(path, "/workspaces/") || strings.Contains(path, "/entities/"))) ||
			(method == http.MethodDelete && (strings.Contains(path, "/entity-hierarchies/") || strings.Contains(path, "/entity-jurisdictions/"))) {
			req.Header.Set("If-Match", `"v1"`)
		}
	}
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

		// ── ORG-02 §4.2 ──────────────────────────────────────────────────────
		{"tenant named command", http.MethodPost, "/v1/tenants/" + tenantID + "/commands/SuspendTenant", `{"reason":"fraud review"}`, http.StatusOK, tenantID},
		{"change default locale", http.MethodPost, "/v1/tenants/" + tenantID + "/defaults", `{"primary_locale":"fr-FR","reason":"customer request"}`, http.StatusOK, tenantID},
		{"lifecycle history", http.MethodGet, "/v1/tenants/" + tenantID + "/lifecycle-history", "", http.StatusOK, tenantID},
		{"get tenant defaults", http.MethodGet, "/v1/tenants/" + tenantID + "/defaults", "", http.StatusOK, tenantID},
		{"change home region", http.MethodPost, "/v1/tenants/" + tenantID + "/home-region", `{"residency_region_id":"0a000000-0000-4000-8000-000000000001","home_region_decision_ref":"RES-1","reason":"r"}`, http.StatusAccepted, tenantID},
		{"bind tenant host", http.MethodPost, "/v1/tenants/" + tenantID + "/host-bindings", `{"hostname":"acme.example.com"}`, http.StatusCreated, tenantID},
		{"list host bindings", http.MethodGet, "/v1/tenants/" + tenantID + "/host-bindings", "", http.StatusOK, tenantID},
		// No wantID: this route takes no path parameter — it is the lookup that
		// establishes which tenant a caller belongs to.
		{"resolve tenant by host", http.MethodGet, "/v1/resolve-tenant?hostname=acme.example.com", "", http.StatusOK, ""},

		// ── ORG-03 §4.3 ──────────────────────────────────────────────────────
		{"amend legal profile", http.MethodPost, "/v1/entities/" + entityID + "/profile-amendments", `{"change_reason":"AMENDMENT"}`, http.StatusCreated, entityID},
		{"change legal name", http.MethodPost, "/v1/entities/" + entityID + "/legal-name", `{"legal_name":"Renamed Ltd"}`, http.StatusCreated, entityID},
		{"change registered office", http.MethodPost, "/v1/entities/" + entityID + "/registered-office", `{"registered_office":"{}"}`, http.StatusCreated, entityID},
		{"list entity versions", http.MethodGet, "/v1/entities/" + entityID + "/versions", "", http.StatusOK, entityID},
		{"get entity as-of", http.MethodGet, "/v1/entities/" + entityID + "/as-of?as_of=2026-03-31", "", http.StatusOK, entityID},
		{"find by registry number", http.MethodGet, "/v1/entities/by-registry-number?registration_number=RC-1", "", http.StatusOK, ""},
		{"list registry conflicts", http.MethodGet, "/v1/registry-conflicts", "", http.StatusOK, ""},
		{"resolve registry conflict", http.MethodPost, "/v1/registry-conflicts/conflict-1/resolution", `{"status":"DISMISSED","resolution_note":"n"}`, http.StatusNoContent, "conflict-1"},

		// ── ORG-03 Draft → Verified → Active; merge ─────────────────────────
		{"request entity verification", http.MethodPost, "/v1/entities/" + entityID + "/verification", `{"verification_evidence_ref":"x"}`, http.StatusAccepted, entityID},
		{"activate legal entity", http.MethodPost, "/v1/entities/" + entityID + "/activation", `{"reason":"r"}`, http.StatusOK, entityID},
		{"merge duplicate candidate", http.MethodPost, "/v1/entities/" + entityID + "/merge", `{"survivor_legal_entity_id":"s","reason":"r"}`, http.StatusAccepted, entityID},
		{"unmerge entity", http.MethodPost, "/v1/entities/" + entityID + "/unmerge", `{"reason":"r"}`, http.StatusAccepted, entityID},
		{"list entity merge records", http.MethodGet, "/v1/entities/" + entityID + "/merge-records", "", http.StatusOK, entityID},

		// ── Verified maker-checker ──────────────────────────────────────────
		{"list approval requests", http.MethodGet, "/v1/approval-requests?status=all", "", http.StatusOK, ""},
		{"get approval request", http.MethodGet, "/v1/approval-requests/apr-1", "", http.StatusOK, "apr-1"},
		{"approve request", http.MethodPost, "/v1/approval-requests/apr-1/approve", `{"payload_fingerprint":"f"}`, http.StatusOK, "apr-1"},
		{"reject request", http.MethodPost, "/v1/approval-requests/apr-1/reject", `{"note":"no"}`, http.StatusOK, "apr-1"},
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
		{"approval required", registry.ErrApprovalRequired, http.StatusUnprocessableEntity},
		{"self approval", registry.ErrSelfApproval, http.StatusForbidden},
		{"approval not pending", registry.ErrApprovalNotPending, http.StatusConflict},
		{"fingerprint mismatch", registry.ErrApprovalFingerprintMismatch, http.StatusConflict},
		{"approval already pending", registry.ErrApprovalPending, http.StatusConflict},
		{"onboarding key required", registry.ErrOnboardingKeyRequired, http.StatusUnprocessableEntity},
		{"entity not operational", registry.ErrEntityNotOperational, http.StatusConflict},
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

// A maker-checker command that was filed rather than executed is a 202 whose
// body carries the approval request — on every route that can file one — and
// never the 200/201 the command's own success would be.
func TestWriteErr_PendingApprovalIs202WithTheRequest(t *testing.T) {
	pending := &registry.PendingApprovalError{Request: &domain.ApprovalRequest{
		ApprovalRequestID: "apr-9", PayloadFingerprint: "fp-9", Status: domain.ApprovalPending,
	}}
	for _, rt := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/tenants/" + tenantID + "/commands/InitiateTermination", `{"reason":"r"}`},
		{http.MethodPost, "/v1/entities/ent-1/legal-name", `{"legal_name":"X"}`},
		{http.MethodPost, "/v1/entities/ent-1/profile-amendments", `{"legal_name":"X"}`},
		{http.MethodPost, "/v1/registry-conflicts/c-1/resolution", `{"status":"DISMISSED","resolution_note":"n"}`},
	} {
		t.Run(rt.path, func(t *testing.T) {
			rec := do(newRouter(&stubSvc{err: pending}), rt.method, rt.path, rt.body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("pending approval mapped to %d, want 202", rec.Code)
			}
			// Check new problem details format
			var body struct {
				Code     string `json:"code"`
				Detail   string `json:"detail"`
				Extra    map[string]any `json:"-"`
			}
			// Parse the full body to check Extra
			var fullBody map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &fullBody); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body.String())
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body.String())
			}
			if body.Code != "pending_approval" {
				t.Fatalf("code = %q, want %q", body.Code, "pending_approval")
			}
			if body.Detail != "Command filed for independent approval" {
				t.Fatalf("detail = %q, want %q", body.Detail, "Command filed for independent approval")
			}
			// Check Extra contains the approval request
			if extra, ok := fullBody["approval_status"].(string); !ok || extra != "PENDING_APPROVAL" {
				t.Fatalf("Extra.approval_status = %v, want %q", fullBody["approval_status"], "PENDING_APPROVAL")
			}
			if approvalReq, ok := fullBody["approval_request"].(map[string]any); !ok {
				t.Fatalf("Extra.approval_request missing or not object")
			} else {
				if approvalReq["approval_request_id"] != "apr-9" {
					t.Fatalf("approval_request.approval_request_id = %v, want %q", approvalReq["approval_request_id"], "apr-9")
				}
				if approvalReq["payload_fingerprint"] != "fp-9" {
					t.Fatalf("approval_request.payload_fingerprint = %v, want %q", approvalReq["payload_fingerprint"], "fp-9")
				}
			}
		})
	}
}

// §8 NP3 used to be checked only by the tenant-command and default-locale
// handlers; every other route served a request on tenant A's hostname that
// claimed tenant B (found live on 28 Sep 2026: GET /v1/tenants/{B} → 200).
func TestNP3_HostGuardRunsBeforeEveryV1Route(t *testing.T) {
	s := &stubSvc{hostTenantErr: registry.ErrHostTenantMismatch}
	r := newRouter(s)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/tenants/" + tenantID},
		{http.MethodGet, "/v1/entities/" + entityID},
		{http.MethodPost, "/v1/entities"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Host = "tenant-a.example.test"
		req = req.WithContext(domain.WithTenant(req.Context(), "tenant-b"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: got %d, want 403", tc.method, tc.path, rec.Code)
		}
		if s.gotHostname != "tenant-a.example.test" || s.gotClaimedTenant != "tenant-b" {
			t.Errorf("%s %s: compared host %q with tenant %q; want the request host and the verified request tenant",
				tc.method, tc.path, s.gotHostname, s.gotClaimedTenant)
		}
	}
}

// A request that names no tenant is not a mismatch — ResolveTenantByHost is
// exactly such a request, and it must keep working.
func TestNP3_HostGuardIgnoresRequestsWithNoTenant(t *testing.T) {
	s := &stubSvc{hostTenantErr: registry.ErrHostTenantMismatch}
	r := newRouter(s)

	req := httptest.NewRequest(http.MethodGet, "/v1/resolve-tenant?hostname=x.example.test", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rec.Code)
	}
}

// ORG §3 "stable typed errors": every refusal carries a code a client can
// branch on, and a refined sentinel gets its own code, not its parent's — a
// stale version must not read as a duplicate (it did, as prose, until 28 Sep
// 2026: "conflict: resource already exists: record was modified …").
func TestWriteErr_EveryRefusalCarriesItsTypedCode(t *testing.T) {
	for _, tc := range []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{fmt.Errorf("%w: you supplied 1, it is at 3", registry.ErrVersionConflict), http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrApprovalFingerprintMismatch, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrRegistryConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrApprovalPending, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrOnboardingKeyMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrInvalidTransition, http.StatusUnprocessableEntity, "INVALID_TRANSITION"},
		{registry.ErrTenantNotTransactable, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrEntityNotOperational, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrApprovalNotPending, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrApprovalRequired, http.StatusUnprocessableEntity, "SOD_DENIED"},
		{registry.ErrSourceUnverified, http.StatusBadRequest, "SOURCE_UNVERIFIED"},
		{registry.ErrReferenceInvalid, http.StatusBadRequest, "REFERENCE_RETIRED"},
		{registry.ErrRegionUnresolved, http.StatusConflict, "RULE_AMBIGUOUS"},
		{registry.ErrServiceUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
		{registry.ErrHostTenantMismatch, http.StatusForbidden, "CONTEXT_INVALID"},
		{registry.ErrUnauthenticated, http.StatusUnauthorized, "CONTEXT_INVALID"},
		{registry.ErrUnauthorized, http.StatusForbidden, "AUTHORIZATION_DENIED"},
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
		{registry.ErrOnboardingKeyRequired, http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
		{errString("pq: boom"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			rec := do(newRouter(&stubSvc{err: tc.err}), http.MethodGet, "/v1/tenants/"+tenantID, "")
			var body struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body.String())
			}
			if rec.Code != tc.wantStatus || body.Code != tc.wantCode {
				t.Fatalf("got %d %q, want %d %q (body %s)", rec.Code, body.Code, tc.wantStatus, tc.wantCode, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
				t.Fatalf("Content-Type = %q, want application/problem+json (API-2)", ct)
			}
			if strings.Contains(body.Detail, "already exists") && tc.wantCode != "DUPLICATE_CANDIDATE" {
				t.Fatalf("a %s must not read as a duplicate: %q", tc.wantCode, body.Detail)
			}
		})
	}
}

// A malformed body has no service error behind it and still gets a code.
func TestWriteErr_MalformedBodyIsValidationFailed(t *testing.T) {
	rec := do(newRouter(&stubSvc{}), http.MethodPost, "/v1/entities", "{")
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest || body.Code != "VALIDATION_FAILED" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// The legacy write routes used to pass an empty correlation id to the
// service whenever the body named none, so their events could not be traced
// to the request (seen live on 28 Sep 2026 in identity-context-svc's log).
func TestLegacyWrites_CarryTheEnvelopeCorrelationID(t *testing.T) {
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, "/v1/entities"},
		{http.MethodPost, "/v1/workspaces"},
		{http.MethodPatch, "/v1/entities/" + entityID},
		{http.MethodPost, "/v1/entity-hierarchies"},
		{http.MethodPost, "/v1/entities/" + entityID + "/jurisdictions"},
	} {
		s := &stubSvc{}
		do(newRouter(s), rt.method, rt.path, `{}`)
		if s.gotCorrID != corrID {
			t.Errorf("%s %s: service saw correlation id %q, want the header's %q", rt.method, rt.path, s.gotCorrID, corrID)
		}
	}
}

// ── API-3 ETag/If-Match tests ──────────────────────────────────────────────────

// TestETag_ReadEndpointsReturnETag verifies that GET endpoints for mutable
// aggregates return an ETag header with the record_version.
func TestETag_ReadEndpointsReturnETag(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"get tenant", http.MethodGet, "/v1/tenants/" + tenantID},
		{"get entity", http.MethodGet, "/v1/entities/" + entityID},
		{"get workspace", http.MethodGet, "/v1/workspaces/" + wsID},
		{"get residency policy", http.MethodGet, "/v1/residency-policies/" + policyID},
		{"get tax bundle", http.MethodGet, "/v1/tax-identity-bundles/" + bundleID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(newRouter(&stubSvc{}), tc.method, tc.path, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			etag := rec.Header().Get("ETag")
			if etag == "" {
				t.Fatal("ETag header is missing")
			}
			if etag != `"v1"` {
				t.Fatalf("ETag = %q, want %q", etag, `"v1"`)
			}
		})
	}
}

// TestIfMatch_MissingReturns428 verifies that mutations of existing aggregates
// require an If-Match header and return 428 Precondition Required when missing.
func TestIfMatch_MissingReturns428(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"tenant lifecycle", http.MethodPost, "/v1/tenants/" + tenantID + "/lifecycle", `{"new_lifecycle_state":"ACTIVE"}`},
		{"update workspace", http.MethodPatch, "/v1/workspaces/" + wsID, `{"name":"W2"}`},
		{"workspace status", http.MethodPost, "/v1/workspaces/" + wsID + "/status", `{"new_status":"ARCHIVED"}`},
		{"update entity", http.MethodPatch, "/v1/entities/" + entityID, `{"legal_name":"New"}`},
		{"transition entity status", http.MethodPost, "/v1/entities/" + entityID + "/status", `{"new_status":"ACTIVE"}`},
		{"end-date hierarchy", http.MethodDelete, "/v1/entity-hierarchies/" + hierID + "?end_date=" + endDateRaw, ""},
		{"end-date jurisdiction", http.MethodDelete, "/v1/entity-jurisdictions/" + assignID + "?end_date=" + endDateRaw, ""},
		{"tax bundle status", http.MethodPost, "/v1/tax-identity-bundles/" + bundleID + "/status", `{"new_status":"ACTIVE"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Correlation-ID", corrID)
			// No If-Match header
			rec := httptest.NewRecorder()
			newRouter(&stubSvc{}).ServeHTTP(rec, req)
			if rec.Code != http.StatusPreconditionRequired {
				t.Fatalf("expected 428, got %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body.String())
			}
			if body.Code != "precondition_required" {
				t.Fatalf("code = %q, want %q", body.Code, "precondition_required")
			}
		})
	}
}

// TestIfMatch_StaleReturns412 verifies that mutations with a stale ETag
// return 412 Precondition Failed.
func TestIfMatch_StaleReturns412(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"tenant lifecycle", http.MethodPost, "/v1/tenants/" + tenantID + "/lifecycle", `{"new_lifecycle_state":"ACTIVE"}`},
		{"update workspace", http.MethodPatch, "/v1/workspaces/" + wsID, `{"name":"W2"}`},
		{"workspace status", http.MethodPost, "/v1/workspaces/" + wsID + "/status", `{"new_status":"ARCHIVED"}`},
		{"update entity", http.MethodPatch, "/v1/entities/" + entityID, `{"legal_name":"New"}`},
		{"transition entity status", http.MethodPost, "/v1/entities/" + entityID + "/status", `{"new_status":"ACTIVE"}`},
		{"end-date hierarchy", http.MethodDelete, "/v1/entity-hierarchies/" + hierID + "?end_date=" + endDateRaw, ""},
		{"end-date jurisdiction", http.MethodDelete, "/v1/entity-jurisdictions/" + assignID + "?end_date=" + endDateRaw, ""},
		{"tax bundle status", http.MethodPost, "/v1/tax-identity-bundles/" + bundleID + "/status", `{"new_status":"ACTIVE"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Correlation-ID", corrID)
			req.Header.Set("If-Match", `"v999"`) // Stale version
			rec := httptest.NewRecorder()
			newRouter(&stubSvc{}).ServeHTTP(rec, req)
			if rec.Code != http.StatusPreconditionFailed {
				t.Fatalf("expected 412, got %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Code           string `json:"code"`
				CurrentVersion int64  `json:"current_version"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body.String())
			}
			if body.Code != "stale_version" {
				t.Fatalf("code = %q, want %q", body.Code, "stale_version")
			}
			if body.CurrentVersion != 1 {
				t.Fatalf("current_version = %d, want 1", body.CurrentVersion)
			}
			// Response should include the current ETag
			etag := rec.Header().Get("ETag")
			if etag != `"v1"` {
				t.Fatalf("response ETag = %q, want %q", etag, `"v1"`)
			}
		})
	}
}

// TestIfMatch_CorrectReturns2xx verifies that mutations with a correct ETag
// succeed with 2xx.
func TestIfMatch_CorrectReturns2xx(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{"tenant lifecycle", http.MethodPost, "/v1/tenants/" + tenantID + "/lifecycle", `{"new_lifecycle_state":"ACTIVE"}`, http.StatusNoContent},
		{"update workspace", http.MethodPatch, "/v1/workspaces/" + wsID, `{"name":"W2"}`, http.StatusOK},
		{"workspace status", http.MethodPost, "/v1/workspaces/" + wsID + "/status", `{"new_status":"ARCHIVED"}`, http.StatusNoContent},
		{"update entity", http.MethodPatch, "/v1/entities/" + entityID, `{"legal_name":"New"}`, http.StatusOK},
		{"transition entity status", http.MethodPost, "/v1/entities/" + entityID + "/status", `{"new_status":"ACTIVE"}`, http.StatusNoContent},
		{"end-date hierarchy", http.MethodDelete, "/v1/entity-hierarchies/" + hierID + "?end_date=" + endDateRaw, "", http.StatusNoContent},
		{"end-date jurisdiction", http.MethodDelete, "/v1/entity-jurisdictions/" + assignID + "?end_date=" + endDateRaw, "", http.StatusNoContent},
		{"tax bundle status", http.MethodPost, "/v1/tax-identity-bundles/" + bundleID + "/status", `{"new_status":"ACTIVE"}`, http.StatusNoContent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Correlation-ID", corrID)
			req.Header.Set("If-Match", `"v1"`) // Correct version
			rec := httptest.NewRecorder()
			newRouter(&stubSvc{}).ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}
			// For 200 responses, verify ETag is returned with updated version
			if tc.wantStatus == http.StatusOK {
				etag := rec.Header().Get("ETag")
				if etag == "" {
					t.Fatal("ETag header is missing on 200 response")
				}
			}
		})
	}
}

// TestIfMatch_WildcardAcceptsAnyVersion verifies that If-Match: * accepts any
// current version (per RFC 7232).
func TestIfMatch_WildcardAcceptsAnyVersion(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, "/v1/entities/"+entityID, strings.NewReader(`{"legal_name":"New"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", corrID)
	req.Header.Set("If-Match", "*")
	rec := httptest.NewRecorder()
	newRouter(&stubSvc{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with If-Match: *, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIfMatch_MalformedReturns400 verifies that a malformed If-Match header
// returns 400 Bad Request.
func TestIfMatch_MalformedReturns400(t *testing.T) {
	req := httptest.NewRequest(http.MethodPatch, "/v1/entities/"+entityID, strings.NewReader(`{"legal_name":"New"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Correlation-ID", corrID)
	req.Header.Set("If-Match", `"vabc"`) // Malformed
	rec := httptest.NewRecorder()
	newRouter(&stubSvc{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if body.Code != "invalid_if_match" {
		t.Fatalf("code = %q, want %q", body.Code, "invalid_if_match")
	}
}
