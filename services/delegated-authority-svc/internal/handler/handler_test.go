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
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
	"zoiko.io/delegated-authority-svc/internal/handler"
	"zoiko.io/delegated-authority-svc/internal/middleware"
)

// ── stubs ─────────────────────────────────────────────────────────────────────

type stubStore struct {
	byID          map[string]*domain.DelegationGrant
	byCorrelation map[string]*domain.DelegationGrant
}

func newStubStore() *stubStore {
	return &stubStore{byID: make(map[string]*domain.DelegationGrant), byCorrelation: make(map[string]*domain.DelegationGrant)}
}

func (s *stubStore) CreateDelegation(_ context.Context, d *domain.DelegationGrant) (bool, error) {
	if existing, ok := s.byCorrelation[d.CorrelationID]; ok {
		*d = *existing
		return false, nil
	}
	cp := *d
	s.byID[d.DelegationID] = &cp
	s.byCorrelation[d.CorrelationID] = &cp
	return true, nil
}

func (s *stubStore) ExpireDue(_ context.Context) ([]domain.DelegationGrant, error) {
	var out []domain.DelegationGrant
	now := time.Now().UTC()
	for _, d := range s.byID {
		if d.Status == domain.DelegationStatusActive && d.EffectiveTo.Before(now) {
			d.Status = domain.DelegationStatusExpired
			d.ExpiredAt = &now
			d.UpdatedAt = now
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *stubStore) GetDelegation(_ context.Context, delegationID string) (*domain.DelegationGrant, error) {
	d, ok := s.byID[delegationID]
	if !ok {
		return nil, domain.ErrDelegationNotFound
	}
	cp := *d
	return &cp, nil
}

func (s *stubStore) ListDelegations(_ context.Context, legalEntityID, delegatorPrincipalID, delegatePrincipalID, status string) ([]domain.DelegationGrant, error) {
	var out []domain.DelegationGrant
	for _, d := range s.byID {
		out = append(out, *d)
	}
	return out, nil
}

func (s *stubStore) RevokeDelegation(_ context.Context, delegationID, revokedByPrincipalID string) (*domain.DelegationGrant, error) {
	d, ok := s.byID[delegationID]
	if !ok {
		return nil, domain.ErrDelegationNotFound
	}
	if d.Status != domain.DelegationStatusActive {
		return nil, domain.ErrInvalidTransition
	}
	now := time.Now().UTC()
	d.Status = domain.DelegationStatusRevoked
	d.RevokedByPrincipalID = &revokedByPrincipalID
	d.RevokedAt = &now
	d.UpdatedAt = now
	cp := *d
	return &cp, nil
}

type stubPublisher struct {
	delegated, revoked, expired int
}

func (p *stubPublisher) PublishDelegated(_ context.Context, _ domain.DelegationGrant) { p.delegated++ }
func (p *stubPublisher) PublishRevoked(_ context.Context, _ domain.DelegationGrant)   { p.revoked++ }
func (p *stubPublisher) PublishExpired(_ context.Context, _ domain.DelegationGrant)   { p.expired++ }

// stubAuthZ grants everything except when delegatorDenied is set — that
// simulates the delegator lacking the authority being delegated, distinct
// from the caller's own authz check (which always succeeds here).
type stubAuthZ struct {
	delegatorDenied string // principal_id that should be denied
}

func (a *stubAuthZ) CheckAllowed(_ context.Context, principalID, _, _ string) error {
	if a.delegatorDenied != "" && principalID == a.delegatorDenied {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

// ── router factory ─────────────────────────────────────────────────────────────

func newRouter(s *stubStore, pub *stubPublisher, authz *stubAuthZ) chi.Router {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			req = req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc"))
			next.ServeHTTP(w, req)
		})
	})
	h := handler.New(s, pub, authz, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func doReq(r chi.Router, method, path string, body any, principalID string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func delegationBody(delegator, correlationID string, from, to time.Time) map[string]any {
	return map[string]any{
		"legal_entity_id":        "le-us",
		"delegator_principal_id": delegator,
		"delegate_principal_id":  "delegate-1",
		"action_type":            "PO_ISSUE",
		"effective_from":         from,
		"effective_to":           to,
		"correlation_id":         correlationID,
	}
}

// ── CreateDelegation tests ─────────────────────────────────────────────────────

func TestCreateDelegation_MissingPrincipal(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour)), "")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 got %d", rr.Code)
	}
}

func TestCreateDelegation_InvalidTimeWindow(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	now := time.Now()
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), now, now), "caller-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 got %d", rr.Code)
	}
}

func TestCreateDelegation_DelegatorLacksAuthority(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{delegatorDenied: "delegator-1"})
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour)), "caller-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestCreateDelegation_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour)), "caller-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201 got %d: %s", rr.Code, rr.Body.String())
	}
	var d domain.DelegationGrant
	_ = json.NewDecoder(rr.Body).Decode(&d)
	if d.Status != domain.DelegationStatusActive {
		t.Errorf("expected ACTIVE got %q", d.Status)
	}
	if pub.delegated != 1 {
		t.Errorf("expected 1 authority.delegated event, got %d", pub.delegated)
	}
}

func TestCreateDelegation_IdempotentReplay(t *testing.T) {
	correlationID := uuid.NewString()
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})

	rr1 := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", correlationID, time.Now(), time.Now().Add(24*time.Hour)), "caller-1")
	var d1 domain.DelegationGrant
	_ = json.NewDecoder(rr1.Body).Decode(&d1)

	rr2 := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", correlationID, time.Now(), time.Now().Add(24*time.Hour)), "caller-1")
	var d2 domain.DelegationGrant
	_ = json.NewDecoder(rr2.Body).Decode(&d2)

	if d2.DelegationID != d1.DelegationID {
		t.Fatalf("retried create resolved to a different delegation_id (%s) than the original (%s)", d2.DelegationID, d1.DelegationID)
	}
}

// ── revoke tests ─────────────────────────────────────────────────────────────

func createActiveDelegation(t *testing.T, r chi.Router) domain.DelegationGrant {
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour)), "caller-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("delegation setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var d domain.DelegationGrant
	_ = json.NewDecoder(rr.Body).Decode(&d)
	return d
}

func TestRevokeDelegation_HappyPath(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})
	d := createActiveDelegation(t, r)

	rr := doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke", nil, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d: %s", rr.Code, rr.Body.String())
	}
	var updated domain.DelegationGrant
	_ = json.NewDecoder(rr.Body).Decode(&updated)
	if updated.Status != domain.DelegationStatusRevoked {
		t.Errorf("expected REVOKED got %q", updated.Status)
	}
	if pub.revoked != 1 {
		t.Errorf("expected 1 authority.revoked event, got %d", pub.revoked)
	}
}

func TestRevokeDelegation_AlreadyRevoked(t *testing.T) {
	r := newRouter(newStubStore(), &stubPublisher{}, &stubAuthZ{})
	d := createActiveDelegation(t, r)
	_ = doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke", nil, "admin-1")

	rr := doReq(r, http.MethodPost, "/v1/delegations/"+d.DelegationID+"/revoke", nil, "admin-1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── lazy expiry tests ──────────────────────────────────────────────────────────

func TestListDelegations_LazilyExpiresDueGrants(t *testing.T) {
	pub := &stubPublisher{}
	r := newRouter(newStubStore(), pub, &stubAuthZ{})
	past := time.Now().Add(-48 * time.Hour)
	rr := doReq(r, http.MethodPost, "/v1/delegations/", delegationBody("delegator-1", uuid.NewString(), past, past.Add(1*time.Hour)), "caller-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("delegation setup failed: %d %s", rr.Code, rr.Body.String())
	}
	var d domain.DelegationGrant
	_ = json.NewDecoder(rr.Body).Decode(&d)

	listRR := doReq(r, http.MethodGet, "/v1/delegations/", nil, "caller-1")
	if listRR.Code != http.StatusOK {
		t.Fatalf("expected 200 got %d", listRR.Code)
	}
	if pub.expired != 1 {
		t.Errorf("expected 1 authority.expired event from the lazy sweep, got %d", pub.expired)
	}

	getRR := doReq(r, http.MethodGet, "/v1/delegations/"+d.DelegationID, nil, "caller-1")
	var fetched domain.DelegationGrant
	_ = json.NewDecoder(getRR.Body).Decode(&fetched)
	if fetched.Status != domain.DelegationStatusExpired {
		t.Errorf("expected EXPIRED got %q", fetched.Status)
	}
}
