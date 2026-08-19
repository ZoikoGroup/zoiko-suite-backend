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

	"zoiko.io/capability-registry-svc/internal/domain"
	"zoiko.io/capability-registry-svc/internal/events"
)

type stubStore struct {
	capabilities map[string]*domain.Capability
	byCode       map[string]*domain.Capability
	marketRel    map[string]*domain.MarketRelease // keyed by capabilityID+marketCode
	integrations map[string][]domain.IntegrationCapability
	releases     map[string][]domain.Release
	claims       map[string][]domain.CapabilityClaim
}

func newStubStore() *stubStore {
	return &stubStore{
		capabilities: make(map[string]*domain.Capability),
		byCode:       make(map[string]*domain.Capability),
		marketRel:    make(map[string]*domain.MarketRelease),
		integrations: make(map[string][]domain.IntegrationCapability),
		releases:     make(map[string][]domain.Release),
		claims:       make(map[string][]domain.CapabilityClaim),
	}
}

func (s *stubStore) CreateCapability(_ context.Context, c *domain.Capability) error {
	if _, exists := s.byCode[c.CapabilityCode]; exists {
		return domain.ErrConflict
	}
	s.capabilities[c.CapabilityID] = c
	s.byCode[c.CapabilityCode] = c
	return nil
}

func (s *stubStore) GetCapability(_ context.Context, id string) (*domain.Capability, error) {
	if c, ok := s.capabilities[id]; ok {
		return c, nil
	}
	return nil, domain.ErrCapabilityNotFound
}

func (s *stubStore) GetCapabilityByCode(_ context.Context, code string) (*domain.Capability, error) {
	if c, ok := s.byCode[code]; ok {
		return c, nil
	}
	return nil, domain.ErrCapabilityNotFound
}

func (s *stubStore) CreateMarketRelease(_ context.Context, m *domain.MarketRelease) error {
	s.marketRel[m.CapabilityID+"|"+m.MarketCode] = m
	return nil
}

func (s *stubStore) GetActiveMarketRelease(_ context.Context, capabilityID, marketCode string) (*domain.MarketRelease, error) {
	if m, ok := s.marketRel[capabilityID+"|"+marketCode]; ok {
		return m, nil
	}
	return nil, domain.ErrMarketReleaseNotFound
}

func (s *stubStore) CreateIntegrationCapability(_ context.Context, i *domain.IntegrationCapability) error {
	s.integrations[i.CapabilityID] = append(s.integrations[i.CapabilityID], *i)
	return nil
}

func (s *stubStore) ListIntegrationCapabilitiesByCapability(_ context.Context, capabilityID string) ([]domain.IntegrationCapability, error) {
	return s.integrations[capabilityID], nil
}

func (s *stubStore) UpdateIntegrationHealth(_ context.Context, id, status string) error {
	for capID, list := range s.integrations {
		for i := range list {
			if list[i].IntegrationCapabilityID == id {
				list[i].HealthStatus = status
				s.integrations[capID] = list
				return nil
			}
		}
	}
	return domain.ErrIntegrationCapabilityNotFound
}

func (s *stubStore) CreateRelease(_ context.Context, r *domain.Release) error {
	s.releases[r.CapabilityID] = append(s.releases[r.CapabilityID], *r)
	return nil
}

func (s *stubStore) GetCurrentRelease(_ context.Context, capabilityID string) (*domain.Release, error) {
	list := s.releases[capabilityID]
	if len(list) == 0 {
		return nil, domain.ErrReleaseNotFound
	}
	latest := list[0]
	for _, r := range list {
		if r.EffectiveFrom.After(latest.EffectiveFrom) {
			latest = r
		}
	}
	return &latest, nil
}

func (s *stubStore) CreateCapabilityClaim(_ context.Context, c *domain.CapabilityClaim) error {
	s.claims[c.CapabilityID] = append(s.claims[c.CapabilityID], *c)
	return nil
}

func (s *stubStore) ListClaimsByCapability(_ context.Context, capabilityID string) ([]domain.CapabilityClaim, error) {
	return s.claims[capabilityID], nil
}

func (s *stubStore) ResolveCapability(ctx context.Context, capabilityCode, marketCode string) (*domain.CapabilityResolution, error) {
	cap, err := s.GetCapabilityByCode(ctx, capabilityCode)
	if err != nil {
		return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "CAPABILITY_UNKNOWN"}, nil
	}
	if release, err := s.GetCurrentRelease(ctx, cap.CapabilityID); err == nil {
		if release.State == domain.ReleaseStateDisabled {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "DISABLED"}, nil
		}
		if release.State == domain.ReleaseStateIncidentRestricted {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "INCIDENT_RESTRICTED"}, nil
		}
	}
	if marketCode != "" {
		m, err := s.GetActiveMarketRelease(ctx, cap.CapabilityID, marketCode)
		if err != nil || m.State == domain.MarketReleaseRestricted || m.State == domain.MarketReleaseSuspended || m.State == domain.MarketReleaseRetired {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED"}, nil
		}
	}
	for _, integ := range s.integrations[cap.CapabilityID] {
		if !integ.Certified || integ.HealthStatus == "FAILED" {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "PROVIDER_UNAVAILABLE"}, nil
		}
	}
	return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: true, ReasonCode: "ENABLED"}, nil
}

type stubPublisher struct{}

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error { return nil }

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{ err error }

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

var _ AuthzChecker = (*stubAuthz)(nil)

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubPublisher{}, &stubAuthz{}, logger)
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
	r := httptest.NewRequest(method, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Principal-Id", "principal-test-01")
	return r
}

func createTestCapability(t *testing.T, r *chi.Mux, code string) *domain.Capability {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
		CapabilityCode:     code,
		ModuleDomain:       "billing",
		ExecutionRiskClass: "LOW",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var c domain.Capability
	_ = json.NewDecoder(w.Body).Decode(&c)
	return &c
}

func TestResolveCapability_UnknownCode(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capability-resolution/NOPE", nil))
	var resolved domain.CapabilityResolution
	_ = json.NewDecoder(w.Body).Decode(&resolved)
	if resolved.Enabled || resolved.ReasonCode != "CAPABILITY_UNKNOWN" {
		t.Fatalf("expected CAPABILITY_UNKNOWN, got %+v", resolved)
	}
}

func TestResolveCapability_EnabledByDefault(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "REPORTING")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capability-resolution/"+cap.CapabilityCode, nil))
	var resolved domain.CapabilityResolution
	_ = json.NewDecoder(w.Body).Decode(&resolved)
	if !resolved.Enabled || resolved.ReasonCode != "ENABLED" {
		t.Fatalf("expected ENABLED, got %+v", resolved)
	}
}

func TestResolveCapability_IncidentRestrictedOverridesEverything(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AI_ASSIST")

	wRelease := httptest.NewRecorder()
	r.ServeHTTP(wRelease, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/release-state", domain.SetReleaseStateRequest{
		State:  "INCIDENT_RESTRICTED",
		Reason: "provider outage",
	}))
	if wRelease.Code != http.StatusCreated {
		t.Fatalf("expected 201 setting release state, got %d — %s", wRelease.Code, wRelease.Body.String())
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capability-resolution/"+cap.CapabilityCode, nil))
	var resolved domain.CapabilityResolution
	_ = json.NewDecoder(w.Body).Decode(&resolved)
	if resolved.Enabled || resolved.ReasonCode != "INCIDENT_RESTRICTED" {
		t.Fatalf("expected INCIDENT_RESTRICTED, got %+v", resolved)
	}
}

func TestResolveCapability_MarketBlockedWhenNoActiveRelease(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "PAYROLL_UK")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capability-resolution/"+cap.CapabilityCode+"?market_code=DE", nil))
	var resolved domain.CapabilityResolution
	_ = json.NewDecoder(w.Body).Decode(&resolved)
	if resolved.Enabled || resolved.ReasonCode != "MARKET_BLOCKED" {
		t.Fatalf("expected MARKET_BLOCKED, got %+v", resolved)
	}
}

func TestResolveCapability_ProviderUnavailable(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "TAX_FILING")

	wInteg := httptest.NewRecorder()
	r.ServeHTTP(wInteg, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "avalara",
		Certified:    false,
	}))
	if wInteg.Code != http.StatusCreated {
		t.Fatalf("expected 201 registering integration, got %d — %s", wInteg.Code, wInteg.Body.String())
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capability-resolution/"+cap.CapabilityCode, nil))
	var resolved domain.CapabilityResolution
	_ = json.NewDecoder(w.Body).Decode(&resolved)
	if resolved.Enabled || resolved.ReasonCode != "PROVIDER_UNAVAILABLE" {
		t.Fatalf("expected PROVIDER_UNAVAILABLE, got %+v", resolved)
	}
}

func TestCreateCapability_DuplicateCodeConflict(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	createTestCapability(t, r, "DUPLICATE_ME")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
		CapabilityCode:     "DUPLICATE_ME",
		ModuleDomain:       "billing",
		ExecutionRiskClass: "LOW",
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}
