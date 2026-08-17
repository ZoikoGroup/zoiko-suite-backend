package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/retention-registry-svc/internal/authz"
	"zoiko.io/retention-registry-svc/internal/domain"
	"zoiko.io/retention-registry-svc/internal/events"
)

type stubStore struct {
	policies []domain.RetentionPolicy
	holds    []domain.LegalHold
}

func (s *stubStore) CreateRetentionPolicy(_ context.Context, p *domain.RetentionPolicy) error {
	s.policies = append(s.policies, *p)
	return nil
}

func compatibleStr(stored, want *string) bool {
	return stored == nil || (want != nil && *stored == *want)
}

func (s *stubStore) FindApplicableRetentionPolicy(_ context.Context, recordClass string, jurisdictionCode, tenantID *string) (*domain.RetentionPolicy, error) {
	var best *domain.RetentionPolicy
	bestScore := -1
	for i := range s.policies {
		p := &s.policies[i]
		if p.RecordClass != recordClass || p.PolicyStatus != "ACTIVE" {
			continue
		}
		if !compatibleStr(p.JurisdictionCode, jurisdictionCode) || !compatibleStr(p.TenantID, tenantID) {
			continue
		}
		score := 0
		if p.JurisdictionCode != nil {
			score++
		}
		if p.TenantID != nil {
			score++
		}
		if score > bestScore {
			bestScore = score
			best = p
		}
	}
	return best, nil
}

func (s *stubStore) CreateLegalHold(_ context.Context, h *domain.LegalHold) error {
	s.holds = append(s.holds, *h)
	return nil
}

func (s *stubStore) FindLegalHoldByID(_ context.Context, id string) (*domain.LegalHold, error) {
	for i := range s.holds {
		if s.holds[i].LegalHoldID == id {
			return &s.holds[i], nil
		}
	}
	return nil, domain.ErrLegalHoldNotFound
}

func (s *stubStore) ReleaseLegalHold(_ context.Context, id, releasedBy, releaseApprovedBy string) (*domain.LegalHold, error) {
	for i := range s.holds {
		if s.holds[i].LegalHoldID == id {
			if s.holds[i].HoldStatus != "ACTIVE" {
				return nil, domain.ErrHoldNotActive
			}
			s.holds[i].HoldStatus = "RELEASED"
			s.holds[i].ReleasedByPrincipalID = &releasedBy
			s.holds[i].ReleaseApprovedByPrincipalID = &releaseApprovedBy
			return &s.holds[i], nil
		}
	}
	return nil, domain.ErrLegalHoldNotFound
}

func (s *stubStore) FindActiveHoldForScope(_ context.Context, recordClass, tenantID, entityRef *string) (*domain.LegalHold, error) {
	var best *domain.LegalHold
	bestScore := -1
	for i := range s.holds {
		h := &s.holds[i]
		if h.HoldStatus != "ACTIVE" {
			continue
		}
		if !compatibleStr(h.RecordClass, recordClass) || !compatibleStr(h.TenantID, tenantID) || !compatibleStr(h.EntityRef, entityRef) {
			continue
		}
		score := 0
		if h.RecordClass != nil {
			score++
		}
		if h.TenantID != nil {
			score++
		}
		if h.EntityRef != nil {
			score++
		}
		if score > bestScore {
			bestScore = score
			best = h
		}
	}
	return best, nil
}

func (s *stubStore) Resolve(ctx context.Context, recordClass string, jurisdictionCode, tenantID, entityRef *string) (*domain.RetentionResolution, error) {
	hold, _ := s.FindActiveHoldForScope(ctx, &recordClass, tenantID, entityRef)
	policy, _ := s.FindApplicableRetentionPolicy(ctx, recordClass, jurisdictionCode, tenantID)
	return &domain.RetentionResolution{Blocked: hold != nil, MatchedHold: hold, ApplicablePolicy: policy}, nil
}

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{ err error }

func (a *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return a.err }

var _ AuthzChecker = (*stubAuthz)(nil)

func newTestHandler() (*Handler, *stubStore, *stubPublisher) {
	logger, _ := zap.NewDevelopment()
	st := &stubStore{}
	pub := &stubPublisher{}
	return New(st, pub, &stubAuthz{}, logger), st, pub
}

func newTestRouter(h *Handler) *chi.Mux {
	r := chi.NewRouter()
	RegisterRoutes(r, h)
	return r
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Principal-Id", "records-officer-1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestResolve_NoHoldNoPolicy(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/retention/resolve?record_class=FINANCIAL_LEDGER", nil))
	var res domain.RetentionResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Blocked || res.MatchedHold != nil || res.ApplicablePolicy != nil {
		t.Fatalf("expected empty resolution for an unknown scope, got %+v", res)
	}
}

func TestCreateRetentionPolicy_ThenResolveFindsIt(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/retention-policies", domain.CreateRetentionPolicyRequest{
		RecordClass:          "FINANCIAL_LEDGER",
		MinRetentionDays:     2555, // 7 years
		LegalRegulatoryBasis: "Tax code SS6001 — 7 year retention",
		EffectiveFrom:        "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}

	wResolve := httptest.NewRecorder()
	r.ServeHTTP(wResolve, buildRequest(http.MethodGet, "/v1/retention/resolve?record_class=FINANCIAL_LEDGER", nil))
	var res domain.RetentionResolution
	_ = json.Unmarshal(wResolve.Body.Bytes(), &res)
	if res.Blocked {
		t.Fatalf("expected not blocked with no hold, got %+v", res)
	}
	if res.ApplicablePolicy == nil || res.ApplicablePolicy.MinRetentionDays != 2555 {
		t.Fatalf("expected the just-created policy to resolve, got %+v", res.ApplicablePolicy)
	}
}

// TestLegalHold_BlocksResolveEvenWithAPermissiveRetentionPolicy proves the
// doc7 §J3 doctrine: a hold blocks regardless of what the retention policy
// says — the two are independent, and a hold always wins if active.
func TestLegalHold_BlocksResolveEvenWithAPermissiveRetentionPolicy(t *testing.T) {
	h, _, pub := newTestHandler()
	r := newTestRouter(h)

	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/retention-policies", domain.CreateRetentionPolicyRequest{
		RecordClass:          "OBLIGATION_EVIDENCE",
		MinRetentionDays:     30,
		LegalRegulatoryBasis: "Internal policy — 30 day minimum",
		EffectiveFrom:        "2026-01-01T00:00:00Z",
	}))

	wHold := httptest.NewRecorder()
	r.ServeHTTP(wHold, buildRequest(http.MethodPost, "/v1/legal-holds", domain.CreateLegalHoldRequest{
		ScopeDescription: "Litigation hold — case #4821",
		Authority:        "Legal Counsel — Case #4821",
		RecordClass:      "OBLIGATION_EVIDENCE",
	}))
	if wHold.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating hold, got %d — %s", wHold.Code, wHold.Body.String())
	}
	if pub.calls != 2 { // policy.created + legal_hold.engaged
		t.Errorf("expected 2 events published, got %d", pub.calls)
	}

	wResolve := httptest.NewRecorder()
	r.ServeHTTP(wResolve, buildRequest(http.MethodGet, "/v1/retention/resolve?record_class=OBLIGATION_EVIDENCE", nil))
	var res domain.RetentionResolution
	_ = json.Unmarshal(wResolve.Body.Bytes(), &res)
	if !res.Blocked || res.MatchedHold == nil {
		t.Fatalf("expected the active hold to block resolution regardless of the retention policy, got %+v", res)
	}
	if res.ApplicablePolicy == nil {
		t.Fatalf("expected the retention policy to still be reported alongside the hold, got %+v", res)
	}
}

func TestReleaseLegalHold_RejectsSecondRelease(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	wHold := httptest.NewRecorder()
	r.ServeHTTP(wHold, buildRequest(http.MethodPost, "/v1/legal-holds", domain.CreateLegalHoldRequest{
		ScopeDescription: "test hold",
		Authority:        "Legal Counsel",
	}))
	var hld domain.LegalHold
	_ = json.Unmarshal(wHold.Body.Bytes(), &hld)

	wRelease := httptest.NewRecorder()
	r.ServeHTTP(wRelease, buildRequest(http.MethodPost, "/v1/legal-holds/"+hld.LegalHoldID+"/release", domain.ReleaseLegalHoldRequest{
		ReleaseApprovedByPrincipalID: "general-counsel-1",
	}))
	if wRelease.Code != http.StatusOK {
		t.Fatalf("expected 200 releasing an active hold, got %d — %s", wRelease.Code, wRelease.Body.String())
	}

	wReleaseAgain := httptest.NewRecorder()
	r.ServeHTTP(wReleaseAgain, buildRequest(http.MethodPost, "/v1/legal-holds/"+hld.LegalHoldID+"/release", domain.ReleaseLegalHoldRequest{
		ReleaseApprovedByPrincipalID: "general-counsel-1",
	}))
	if wReleaseAgain.Code != http.StatusConflict {
		t.Fatalf("expected 409 on second release, got %d — %s", wReleaseAgain.Code, wReleaseAgain.Body.String())
	}
}

func TestCreateLegalHold_AuthorizationDenied403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(&stubStore{}, &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/legal-holds", domain.CreateLegalHoldRequest{
		ScopeDescription: "should be denied",
		Authority:        "nobody",
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
}
