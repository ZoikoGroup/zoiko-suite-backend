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
	"zoiko.io/commercial-account-svc/internal/events"
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

func (s *stubStore) GetCommercialAccount(_ context.Context, id string) (*domain.CommercialAccount, error) {
	if a, ok := s.accounts[id]; ok {
		return a, nil
	}
	return nil, domain.ErrCommercialAccountNotFound
}

func (s *stubStore) GetCommercialAccountByOrganization(_ context.Context, organizationID string) (*domain.CommercialAccount, error) {
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

func (s *stubStore) GetMembership(_ context.Context, id string) (*domain.Membership, error) {
	if m, ok := s.memberships[id]; ok {
		return m, nil
	}
	return nil, domain.ErrMembershipNotFound
}

func (s *stubStore) ListMembershipsByOrganization(_ context.Context, organizationID string) ([]domain.Membership, error) {
	var out []domain.Membership
	for _, m := range s.memberships {
		if m.OrganizationID == organizationID {
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

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

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
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, logger)
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
	h := New(newStubStore(), &stubPublisher{}, &stubAuthz{err: authz.ErrAuthorizationDenied}, logger)
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

func TestCreateMembershipAndDeactivate(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	body := domain.CreateMembershipRequest{
		PrincipalID:    "adviser-1",
		OrganizationID: "org-test-01",
	}
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/memberships", body))
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", wCreate.Code, wCreate.Body.String())
	}
	var created domain.Membership
	_ = json.NewDecoder(wCreate.Body).Decode(&created)
	if created.Status != domain.MembershipStatusActive {
		t.Fatalf("expected ACTIVE, got %s", created.Status)
	}

	wDeactivate := httptest.NewRecorder()
	r.ServeHTTP(wDeactivate, buildRequest(http.MethodDelete, "/v1/memberships/"+created.MembershipID, nil))
	if wDeactivate.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d — %s", wDeactivate.Code, wDeactivate.Body.String())
	}

	// Deactivating again must fail — a membership row is never re-usable
	// once ended, per doc7 §A6 (historical attribution, not a toggle).
	wSecond := httptest.NewRecorder()
	r.ServeHTTP(wSecond, buildRequest(http.MethodDelete, "/v1/memberships/"+created.MembershipID, nil))
	if wSecond.Code != http.StatusConflict {
		t.Fatalf("expected 409 on second deactivate, got %d", wSecond.Code)
	}
}

func TestListMemberships(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	body := domain.CreateMembershipRequest{
		PrincipalID:    "adviser-2",
		OrganizationID: "org-test-02",
	}
	wCreate := httptest.NewRecorder()
	req := buildRequest(http.MethodPost, "/v1/memberships", body)
	req.Header.Set("X-Tenant-Id", "org-test-02")
	r.ServeHTTP(wCreate, req)
	if wCreate.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", wCreate.Code)
	}

	wList := httptest.NewRecorder()
	r.ServeHTTP(wList, buildRequest(http.MethodGet, "/v1/organizations/org-test-02/memberships", nil))
	if wList.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wList.Code)
	}
	var resp struct {
		Memberships []domain.Membership `json:"memberships"`
		Total       int                 `json:"total"`
	}
	if err := json.NewDecoder(wList.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("expected 1 membership, got %d", resp.Total)
	}
}

func TestGetCommercialAccount_NotFound(t *testing.T) {
	h := newTestHandler()
	r := chi.NewRouter()
	RegisterRoutes(r, h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/commercial-accounts/"+uuid.NewString(), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
