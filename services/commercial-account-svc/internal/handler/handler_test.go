package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
)

type stubStore struct {
	accounts    map[string]*domain.CommercialAccount
	memberships map[string]*domain.Membership
	sub         *subscriptionStubData
}

func newStubStore() *stubStore {
	return &stubStore{
		accounts:    make(map[string]*domain.CommercialAccount),
		memberships: make(map[string]*domain.Membership),
		sub:         newSubscriptionStubData(),
	}
}

func (s *stubStore) CreateCommercialAccount(_ context.Context, a *domain.CommercialAccount) error {
	if a.CommercialAccountID == "" {
		a.CommercialAccountID = "ca-test-001"
	}
	s.accounts[a.CommercialAccountID] = a
	return nil
}

// The tenant-scoped reads below mirror PgStore's scoping deliberately.
// They previously took `_ context.Context` and ignored it, which meant this
// stub could not enforce a tenant boundary even in principle — so any
// handler test asserting isolation was asserting against a fake that could
// not fail. Taking ctx and applying the same predicate is what makes those
// assertions mean something about production.
func (s *stubStore) GetCommercialAccount(ctx context.Context, id string) (*domain.CommercialAccount, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if a, ok := s.accounts[id]; ok && a.OrganizationID == tenantID {
		return a, nil
	}
	return nil, domain.ErrCommercialAccountNotFound
}

func (s *stubStore) GetCommercialAccountByOrganization(ctx context.Context, organizationID string) (*domain.CommercialAccount, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if tenantID == "" || tenantID != organizationID {
		return nil, domain.ErrCommercialAccountNotFound
	}
	for _, a := range s.accounts {
		if a.OrganizationID == organizationID {
			return a, nil
		}
	}
	return nil, domain.ErrCommercialAccountNotFound
}

func (s *stubStore) CreateMembership(_ context.Context, m *domain.Membership) error {
	if m.MembershipID == "" {
		m.MembershipID = "mem-test-001"
	}
	s.memberships[m.MembershipID] = m
	return nil
}

func (s *stubStore) GetMembership(ctx context.Context, id string) (*domain.Membership, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	if m, ok := s.memberships[id]; ok && m.OrganizationID == tenantID {
		return m, nil
	}
	return nil, domain.ErrMembershipNotFound
}

func (s *stubStore) ListMemberships(ctx context.Context) ([]domain.Membership, error) {
	tenantID := svcmiddleware.TenantFromContext(ctx)
	var out []domain.Membership
	for _, m := range s.memberships {
		if m.OrganizationID == tenantID {
			out = append(out, *m)
		}
	}
	return out, nil
}

func (s *stubStore) DeactivateMembership(_ context.Context, id, organizationID string) error {
	m, ok := s.memberships[id]
	if !ok || m.OrganizationID != organizationID || m.Status != domain.MembershipStatusActive {
		return domain.ErrMembershipNotFound
	}
	m.Status = domain.MembershipStatusDeactivated
	return nil
}

// stubAuthz is a test double for AuthzChecker. By default it grants every
// request; tests that need to exercise the deny/unavailable paths can flip
// err to authz.ErrAuthorizationDenied or authz.ErrAuthzServiceUnavailable.
type stubAuthz struct {
	err error
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error {
	return s.err
}

var _ AuthzChecker = (*stubAuthz)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubAuthz{}, logger)
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tenant-Id", "org-test-01")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	return r
}

func TestCreateCommercialAccount(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCommercialAccountRequest{
		OrganizationID:      "org-test-01",
		LegalCustomerName:   "Verify Co",
		BillingCurrencyCode: "USD",
	}
	w := httptest.NewRecorder()
	h.CreateCommercialAccount(w, buildRequest(http.MethodPost, "/v1/commercial-accounts", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var resp domain.CommercialAccount
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Status != domain.CommercialAccountStatusActive {
		t.Errorf("expected ACTIVE, got %s", resp.Status)
	}
	if resp.LegalCustomerName != "Verify Co" {
		t.Errorf("unexpected legal_customer_name: %s", resp.LegalCustomerName)
	}
}

func TestCreateCommercialAccount_MissingPrincipalHeader(t *testing.T) {
	h := newTestHandler()
	body := domain.CreateCommercialAccountRequest{
		OrganizationID:      "org-test-01",
		LegalCustomerName:   "Verify Co",
		BillingCurrencyCode: "USD",
	}
	r := buildRequest(http.MethodPost, "/v1/commercial-accounts", body)
	r.Header.Del("X-Principal-Id")
	w := httptest.NewRecorder()
	h.CreateCommercialAccount(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestCreateCommercialAccount_AuthzDenied(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubAuthz{err: authz.ErrAuthorizationDenied}, logger)
	body := domain.CreateCommercialAccountRequest{
		OrganizationID:      "org-test-01",
		LegalCustomerName:   "Verify Co",
		BillingCurrencyCode: "USD",
	}
	w := httptest.NewRecorder()
	h.CreateCommercialAccount(w, buildRequest(http.MethodPost, "/v1/commercial-accounts", body))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestGetCommercialAccount_NotFound(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/commercial-accounts/"+uuid.NewString(), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestReadEndpoints_AuthzDenied_Refused(t *testing.T) {
	store := newStubStore()
	store.accounts["ca-test-001"] = &domain.CommercialAccount{
		CommercialAccountID: "ca-test-001",
		OrganizationID:      "org-test-01",
		Status:              domain.CommercialAccountStatusActive,
	}
	store.memberships["mem-test-001"] = &domain.Membership{
		MembershipID:   "mem-test-001",
		OrganizationID: "org-test-01",
		PrincipalID:    "principal-test-01",
		Status:         domain.MembershipStatusActive,
	}
	deniedAuthz := &stubAuthz{err: authz.ErrAuthorizationDenied}
	logger, _ := zap.NewDevelopment()
	h := New(store, deniedAuthz, logger)
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"get commercial account", http.MethodGet, "/v1/commercial-accounts/ca-test-001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(tc.method, tc.path, nil))
			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 Forbidden on authz denial, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestReadEndpoints_MissingPrincipal_Refused(t *testing.T) {
	store := newStubStore()
	store.accounts["ca-test-001"] = &domain.CommercialAccount{
		CommercialAccountID: "ca-test-001",
		OrganizationID:      "org-test-01",
		Status:              domain.CommercialAccountStatusActive,
	}
	store.memberships["mem-test-001"] = &domain.Membership{
		MembershipID:   "mem-test-001",
		OrganizationID: "org-test-01",
		PrincipalID:    "principal-test-01",
		Status:         domain.MembershipStatusActive,
	}
	logger, _ := zap.NewDevelopment()
	h := New(store, &stubAuthz{}, logger)
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	RegisterRoutes(r, h)

	for _, tc := range []struct {
		name, method, path string
	}{
		{"get commercial account", http.MethodGet, "/v1/commercial-accounts/ca-test-001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := buildRequest(tc.method, tc.path, nil)
			req.Header.Del("X-Principal-Id")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 Unauthorized with missing X-Principal-Id, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
