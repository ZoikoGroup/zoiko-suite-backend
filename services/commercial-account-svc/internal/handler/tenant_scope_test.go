package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

// Tenant-scope tests for commercial-account-svc (tracker row 11a).
//
// This service's reads were exposed by two distinct defects that compose:
//
//  1. middleware.TenantContext lets a request with no X-Tenant-Id through
//     (it must — the price-catalog and plan routes are platform-scope and
//     legitimately carry no tenant), and
//  2. the store wrote its tenant predicate as ($2 = '' OR
//     organization_id::text = $2), where $2 is the tenant from context.
//
// So a header-less request disabled its own tenant predicate. Neither the
// GET account nor the GET membership route has an authz check to fall back
// on, so that was an open read of any account or membership by id.
//
// Separately, ListMemberships filtered on the {organizationID} URL path
// with no comparison to the verified tenant and no authz check at all —
// caller-declared tenant identity with nothing in front of it.

func newScopeRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)
	return r
}

func seedMembership(t *testing.T, r *chi.Mux, organizationID, principalID string) domain.Membership {
	t.Helper()
	body, _ := json.Marshal(domain.CreateMembershipRequest{
		PrincipalID:    principalID,
		OrganizationID: organizationID,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/memberships", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", organizationID)
	req.Header.Set("X-Principal-Id", "principal-seed")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed membership for %s: expected 201, got %d — %s", organizationID, w.Code, w.Body.String())
	}
	var m domain.Membership
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode seeded membership: %v", err)
	}
	return m
}

// TestMissingTenantHeader_Refused covers defect 1: a request with no
// X-Tenant-Id used to reach the store, where the tenant predicate then
// disabled itself.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r := newScopeRouter(newTestHandler())

	for _, tc := range []struct {
		name, method, path string
	}{
		{"get commercial account", http.MethodGet, "/v1/commercial-accounts/ca-test-001"},
		{"get membership", http.MethodGet, "/v1/memberships/mem-test-001"},
		{"list memberships", http.MethodGet, "/v1/organizations/org-a/memberships"},
		{"deactivate membership", http.MethodDelete, "/v1/memberships/mem-test-001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			// Deliberately NO X-Tenant-Id.
			req.Header.Set("X-Principal-Id", "principal-test-01")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestListMemberships_ForeignOrganization_Refused covers defect 2, the
// worst of the two: the {organizationID} path segment went straight to the
// store, so any caller could enumerate any organization's roster —
// principal ids, workspace and legal-entity ids, roles, effective dates.
func TestListMemberships_ForeignOrganization_Refused(t *testing.T) {
	r := newScopeRouter(newTestHandler())

	seedMembership(t, r, "org-a", "adviser-a")

	// Tenant B asks for org-a's roster, carrying its own verified identity.
	req := httptest.NewRequest(http.MethodGet, "/v1/organizations/org-a/memberships", nil)
	req.Header.Set("X-Tenant-Id", "org-b")
	req.Header.Set("X-Principal-Id", "principal-b")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for a foreign organization in the path, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("adviser-a")) {
		t.Fatalf("ISOLATION FAILURE: org-b's response leaked org-a's member: %s", w.Body.String())
	}
}

// TestListMemberships_OwnOrganization_Scoped is the other half: the roster
// a tenant does get back must contain only its own rows. Without this, a
// policy that returns everything would still pass the test above.
func TestListMemberships_OwnOrganization_Scoped(t *testing.T) {
	r := newScopeRouter(newTestHandler())

	seedMembership(t, r, "org-a", "adviser-a")
	seedMembership(t, r, "org-b", "adviser-b")

	req := httptest.NewRequest(http.MethodGet, "/v1/organizations/org-b/memberships", nil)
	req.Header.Set("X-Tenant-Id", "org-b")
	req.Header.Set("X-Principal-Id", "principal-b")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for own organization, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Memberships []domain.Membership `json:"memberships"`
		Total       int                 `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("expected exactly org-b's 1 membership, got %d: %+v", resp.Total, resp.Memberships)
	}
	for _, m := range resp.Memberships {
		if m.OrganizationID != "org-b" {
			t.Fatalf("ISOLATION FAILURE: org-b's roster contained organization %q", m.OrganizationID)
		}
	}
}

// TestGetMembership_ForeignTenant_NotFound covers the by-id read. It must
// answer 404, not 403: a distinct "forbidden" would confirm to a prober
// that another tenant's membership id exists.
func TestGetMembership_ForeignTenant_NotFound(t *testing.T) {
	r := newScopeRouter(newTestHandler())

	m := seedMembership(t, r, "org-a", "adviser-a")

	req := httptest.NewRequest(http.MethodGet, "/v1/memberships/"+m.MembershipID, nil)
	req.Header.Set("X-Tenant-Id", "org-b")
	req.Header.Set("X-Principal-Id", "principal-b")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's membership, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("adviser-a")) {
		t.Fatalf("ISOLATION FAILURE: org-b read org-a's membership: %s", w.Body.String())
	}

	// Sanity: org-a still reads its own.
	ownReq := httptest.NewRequest(http.MethodGet, "/v1/memberships/"+m.MembershipID, nil)
	ownReq.Header.Set("X-Tenant-Id", "org-a")
	ownReq.Header.Set("X-Principal-Id", "principal-a")
	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, ownReq)
	if ownW.Code != http.StatusOK {
		t.Fatalf("org-a must still read its own membership, got %d: %s", ownW.Code, ownW.Body.String())
	}
}

// TestDeactivateMembership_ForeignTenant_Refused covers the write path.
// DeactivateMembership fetches the row through the now-scoped GetMembership
// before authorizing, so a foreign membership fails closed at the fetch.
func TestDeactivateMembership_ForeignTenant_Refused(t *testing.T) {
	r := newScopeRouter(newTestHandler())

	m := seedMembership(t, r, "org-a", "adviser-a")

	req := httptest.NewRequest(http.MethodDelete, "/v1/memberships/"+m.MembershipID, nil)
	req.Header.Set("X-Tenant-Id", "org-b")
	req.Header.Set("X-Principal-Id", "principal-b")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 deactivating another tenant's membership, got %d: %s", w.Code, w.Body.String())
	}

	// And org-a's membership must still be ACTIVE — the refusal above has
	// to mean "nothing happened", not just "the response said no".
	ownReq := httptest.NewRequest(http.MethodGet, "/v1/memberships/"+m.MembershipID, nil)
	ownReq.Header.Set("X-Tenant-Id", "org-a")
	ownReq.Header.Set("X-Principal-Id", "principal-a")
	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, ownReq)
	var got domain.Membership
	if err := json.NewDecoder(ownW.Body).Decode(&got); err != nil {
		t.Fatalf("decode org-a's membership: %v", err)
	}
	if got.Status != domain.MembershipStatusActive {
		t.Fatalf("ISOLATION FAILURE: org-b's refused request still changed org-a's membership to %s", got.Status)
	}
}
