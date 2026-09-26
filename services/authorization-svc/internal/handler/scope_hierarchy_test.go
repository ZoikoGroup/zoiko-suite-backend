package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// scopeTrackingStore simulates book and org-unit scope evaluation for handler testing.
type scopeTrackingStore struct {
	stubStore

	grantBookID    string
	grantOrgUnitID string
	actions        []string

	lastBookArg    string
	lastOrgUnitArg string

	lastCreatedAssignment domain.CreateRoleAssignmentParams
	lastCreatedDelegation domain.CreateDelegatedAuthorityParams
}

func (s *scopeTrackingStore) FindGrantedActionsScoped(_ context.Context, _, _, tenantID, bookID, orgUnitID string) ([]string, string, error) {
	s.grantedTenantArg = tenantID
	s.lastBookArg = bookID
	s.lastOrgUnitArg = orgUnitID

	// Hierarchical scope matching:
	// If grant has specific book, it matches only when request specifies that exact book.
	// If grant has empty book, it matches any book.
	if s.grantBookID != "" && s.grantBookID != bookID {
		return nil, "", nil
	}
	if s.grantOrgUnitID != "" && s.grantOrgUnitID != orgUnitID {
		return nil, "", nil
	}

	return s.actions, "rbac:role=POSTER", nil
}

func (s *scopeTrackingStore) FindDelegatedActionsScoped(_ context.Context, _, _, tenantID, bookID, orgUnitID string) ([]string, string, error) {
	s.delegatedTenantArg = tenantID
	if s.grantBookID != "" && s.grantBookID != bookID {
		return nil, "", nil
	}
	if s.grantOrgUnitID != "" && s.grantOrgUnitID != orgUnitID {
		return nil, "", nil
	}
	return s.actions, "delegated:from=controller", nil
}

func (s *scopeTrackingStore) CreateRoleAssignment(_ context.Context, params domain.CreateRoleAssignmentParams) (*domain.PrincipalRoleAssignment, error) {
	s.lastCreatedAssignment = params
	return &domain.PrincipalRoleAssignment{
		PrincipalRoleAssignmentID: "pra-123",
		PrincipalID:               params.PrincipalID,
		RoleID:                    params.RoleID,
		LegalEntityID:             params.LegalEntityID,
		BookID:                    params.BookID,
		OrgUnitID:                 params.OrgUnitID,
		EffectiveFrom:             params.EffectiveFrom,
	}, nil
}

func (s *scopeTrackingStore) CreateDelegatedAuthority(_ context.Context, params domain.CreateDelegatedAuthorityParams) (*domain.DelegatedAuthority, error) {
	s.lastCreatedDelegation = params
	return &domain.DelegatedAuthority{
		DelegatedAuthorityID: "da-123",
		TenantID:             params.TenantID,
		DelegatorPrincipalID: params.DelegatorPrincipalID,
		DelegatePrincipalID:  params.DelegatePrincipalID,
		ScopeType:            params.ScopeType,
		LegalEntityID:        params.LegalEntityID,
		BookID:               params.BookID,
		OrgUnitID:            params.OrgUnitID,
		EffectiveFrom:        params.EffectiveFrom,
	}, nil
}

func newScopeRouter(s *scopeTrackingStore) chi.Router {
	r := chi.NewRouter()
	h := handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func TestAuthorize_ScenarioA03_CrossBookPostingDenial(t *testing.T) {
	bookA := "00000000-0000-0000-0000-0000000000b1"
	bookB := "00000000-0000-0000-0000-0000000000b2"

	st := &scopeTrackingStore{
		grantBookID: bookA,
		actions:     []string{"gl.journal.post"},
	}
	r := newScopeRouter(st)

	// 1. Authorize against Book A (same entity, matching book) -> GRANTED
	bodyA, _ := json.Marshal(map[string]any{
		"principal_id":    "user-1",
		"legal_entity_id": "00000000-0000-0000-0000-0000000000e1",
		"book_id":         bookA,
		"action_type":     "gl.journal.post",
	})
	reqA := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewReader(bodyA))
	reqA.Header.Set("Content-Type", "application/json")
	reqA.Header.Set("X-Tenant-Id", "00000000-0000-0000-0000-000000000001")
	reqA.Header.Set("X-Principal-Id", "user-1")

	recA := httptest.NewRecorder()
	r.ServeHTTP(recA, reqA)

	if recA.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for Book A, got %d: %s", recA.Code, recA.Body.String())
	}
	var respA map[string]any
	if err := json.Unmarshal(recA.Body.Bytes(), &respA); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if respA["decision_outcome"] != "GRANTED" {
		t.Fatalf("expected GRANTED for Book A, got %v (basis: %v)", respA["decision_outcome"], respA["decision_basis"])
	}

	// 2. Scenario A03: Authorize against Book B (same entity, wrong book) -> DENIED
	bodyB, _ := json.Marshal(map[string]any{
		"principal_id":    "user-1",
		"legal_entity_id": "00000000-0000-0000-0000-0000000000e1",
		"book_id":         bookB,
		"action_type":     "gl.journal.post",
	})
	reqB := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewReader(bodyB))
	reqB.Header.Set("Content-Type", "application/json")
	reqB.Header.Set("X-Tenant-Id", "00000000-0000-0000-0000-000000000001")
	reqB.Header.Set("X-Principal-Id", "user-1")

	recB := httptest.NewRecorder()
	r.ServeHTTP(recB, reqB)

	if recB.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for Book B, got %d: %s", recB.Code, recB.Body.String())
	}
	var respB map[string]any
	if err := json.Unmarshal(recB.Body.Bytes(), &respB); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if respB["decision_outcome"] != "DENIED" {
		t.Fatalf("Scenario A03 violation: expected DENIED for Book B, got %v (basis: %v)", respB["decision_outcome"], respB["decision_basis"])
	}
	if respB["decision_basis"] != "no_grant" {
		t.Errorf("expected basis no_grant, got %v", respB["decision_basis"])
	}
}

func TestAuthorize_HierarchicalScope_HeaderFallback(t *testing.T) {
	bookA := "00000000-0000-0000-0000-0000000000b1"
	orgUnit1 := "00000000-0000-0000-0000-0000000000u1"

	st := &scopeTrackingStore{
		grantBookID:    bookA,
		grantOrgUnitID: orgUnit1,
		actions:        []string{"invoice.approve"},
	}
	r := newScopeRouter(st)

	// Body does NOT contain book_id or org_unit_id, but headers X-Book-Id and X-Org-Unit-Id do
	body, _ := json.Marshal(map[string]any{
		"principal_id":    "user-1",
		"legal_entity_id": "00000000-0000-0000-0000-0000000000e1",
		"action_type":     "invoice.approve",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/authorize", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "00000000-0000-0000-0000-000000000001")
	req.Header.Set("X-Principal-Id", "user-1")
	req.Header.Set("X-Book-Id", bookA)
	req.Header.Set("X-Org-Unit-Id", orgUnit1)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if st.lastBookArg != bookA {
		t.Errorf("expected lastBookArg=%s, got %s", bookA, st.lastBookArg)
	}
	if st.lastOrgUnitArg != orgUnit1 {
		t.Errorf("expected lastOrgUnitArg=%s, got %s", orgUnit1, st.lastOrgUnitArg)
	}
}

func TestCreateRoleAssignment_WithBookAndOrgUnit(t *testing.T) {
	bookA := "00000000-0000-0000-0000-0000000000b1"
	orgUnit1 := "00000000-0000-0000-0000-0000000000u1"
	tenantID := "00000000-0000-0000-0000-000000000001"
	roleID := "00000000-0000-0000-0000-0000000000r1"

	st := &scopeTrackingStore{}
	st.role = &domain.Role{RoleID: roleID, TenantID: tenantID, RoleScopeType: "LEGAL_ENTITY"}
	r := newScopeRouter(st)

	body, _ := json.Marshal(map[string]any{
		"principal_id":    "user-1",
		"role_id":         roleID,
		"legal_entity_id": "00000000-0000-0000-0000-0000000000e1",
		"book_id":         bookA,
		"org_unit_id":     orgUnit1,
		"effective_from":  time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/role-assignments", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "admin-1")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	if st.lastCreatedAssignment.BookID == nil || *st.lastCreatedAssignment.BookID != bookA {
		t.Errorf("expected BookID=%s, got %v", bookA, st.lastCreatedAssignment.BookID)
	}
	if st.lastCreatedAssignment.OrgUnitID == nil || *st.lastCreatedAssignment.OrgUnitID != orgUnit1 {
		t.Errorf("expected OrgUnitID=%s, got %v", orgUnit1, st.lastCreatedAssignment.OrgUnitID)
	}
}

func TestCreateDelegatedAuthority_WithBookAndOrgUnit(t *testing.T) {
	bookA := "00000000-0000-0000-0000-0000000000b1"
	orgUnit1 := "00000000-0000-0000-0000-0000000000u1"
	tenantID := "00000000-0000-0000-0000-000000000001"
	callerID := "user-1"

	st := &scopeTrackingStore{}
	r := newScopeRouter(st)

	body, _ := json.Marshal(map[string]any{
		"delegator_principal_id": callerID,
		"delegate_principal_id":  "delegate-1",
		"scope_type":             "FULL",
		"legal_entity_id":        "00000000-0000-0000-0000-0000000000e1",
		"book_id":                bookA,
		"org_unit_id":            orgUnit1,
		"effective_from":         time.Now(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/delegated-authorities", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", callerID)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	if st.lastCreatedDelegation.BookID == nil || *st.lastCreatedDelegation.BookID != bookA {
		t.Errorf("expected BookID=%s, got %v", bookA, st.lastCreatedDelegation.BookID)
	}
	if st.lastCreatedDelegation.OrgUnitID == nil || *st.lastCreatedDelegation.OrgUnitID != orgUnit1 {
		t.Errorf("expected OrgUnitID=%s, got %v", orgUnit1, st.lastCreatedDelegation.OrgUnitID)
	}
}
