package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	authzpkg "zoiko.io/capability-registry-svc/internal/authz"
	"zoiko.io/capability-registry-svc/internal/domain"
	"zoiko.io/capability-registry-svc/internal/events"
)

type stubStore struct {
	idempotencyMu sync.Mutex
	idempotency   map[stubIdempotencyKey]stubIdempotencyResult
	capabilities  map[string]*domain.Capability
	byCode        map[string]*domain.Capability
	marketRel     map[string]*domain.MarketRelease // keyed by capabilityID+marketCode
	integrations  map[string][]domain.IntegrationCapability
	releases      map[string][]domain.Release
	claims        map[string][]domain.CapabilityClaim
	published     []events.PublishParams
}

type stubIdempotencyKey struct {
	tenantID  string
	operation string
	principal string
	key       string
}

type stubIdempotencyResult struct {
	requestHash string
	resourceID  string
	status      int
	body        []byte
}

func newStubStore() *stubStore {
	return &stubStore{
		idempotency:  make(map[stubIdempotencyKey]stubIdempotencyResult),
		capabilities: make(map[string]*domain.Capability),
		byCode:       make(map[string]*domain.Capability),
		marketRel:    make(map[string]*domain.MarketRelease),
		integrations: make(map[string][]domain.IntegrationCapability),
		releases:     make(map[string][]domain.Release),
		claims:       make(map[string][]domain.CapabilityClaim),
	}
}

func (s *stubStore) withIdempotency(claim domain.IdempotencyClaim, write func() error) error {
	s.idempotencyMu.Lock()
	defer s.idempotencyMu.Unlock()

	key := stubIdempotencyKey{claim.TenantID, claim.Operation, claim.PrincipalID, claim.Key}
	if prior, ok := s.idempotency[key]; ok {
		if prior.requestHash != claim.RequestSHA256 {
			return domain.ErrIdempotencyKeyReused
		}
		return &domain.IdempotentReplayError{
			ResourceID: prior.resourceID, ResponseStatus: prior.status, ResponseBody: prior.body,
		}
	}
	if err := write(); err != nil {
		return err
	}
	if len(claim.OutboxPayload) > 0 {
		var event events.Event
		if err := json.Unmarshal(claim.OutboxPayload, &event); err != nil {
			return err
		}
		s.published = append(s.published, events.PublishParams{
			EventType: event.EventType, EntityID: event.EntityID, TenantID: event.TenantID,
			ActorID: event.ActorID, CorrelationID: event.CorrelationID, Payload: event.Payload,
		})
	}
	s.idempotency[key] = stubIdempotencyResult{
		requestHash: claim.RequestSHA256, resourceID: claim.ResourceID,
		status: claim.ResponseStatus, body: append([]byte(nil), claim.ResponseBody...),
	}
	return nil
}

func (s *stubStore) CreateCapability(_ context.Context, c *domain.Capability, claim domain.IdempotencyClaim) error {
	return s.withIdempotency(claim, func() error {
		if _, exists := s.byCode[c.CapabilityCode]; exists {
			return domain.ErrConflict
		}
		s.capabilities[c.CapabilityID] = c
		s.byCode[c.CapabilityCode] = c
		return nil
	})
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

func (s *stubStore) CreateMarketRelease(_ context.Context, m *domain.MarketRelease, claim domain.IdempotencyClaim) error {
	return s.withIdempotency(claim, func() error {
		if _, ok := s.capabilities[m.CapabilityID]; !ok {
			return domain.ErrCapabilityNotFound
		}
		s.marketRel[m.CapabilityID+"|"+m.MarketCode] = m
		return nil
	})
}

func (s *stubStore) GetActiveMarketRelease(_ context.Context, capabilityID, marketCode string) (*domain.MarketRelease, error) {
	if m, ok := s.marketRel[capabilityID+"|"+marketCode]; ok {
		return m, nil
	}
	return nil, domain.ErrMarketReleaseNotFound
}

func (s *stubStore) GetMarketReleaseByID(_ context.Context, id string) (*domain.MarketRelease, error) {
	for _, release := range s.marketRel {
		if release.MarketReleaseID == id {
			return release, nil
		}
	}
	return nil, domain.ErrMarketReleaseNotFound
}

func (s *stubStore) CreateIntegrationCapability(_ context.Context, i *domain.IntegrationCapability, claim domain.IdempotencyClaim) error {
	return s.withIdempotency(claim, func() error {
		if _, ok := s.capabilities[i.CapabilityID]; !ok {
			return domain.ErrCapabilityNotFound
		}
		s.integrations[i.CapabilityID] = append(s.integrations[i.CapabilityID], *i)
		return nil
	})
}

func (s *stubStore) ListIntegrationCapabilitiesByCapability(_ context.Context, capabilityID string) ([]domain.IntegrationCapability, error) {
	return s.integrations[capabilityID], nil
}

func (s *stubStore) GetIntegrationCapabilityByID(_ context.Context, id string) (*domain.IntegrationCapability, error) {
	for _, list := range s.integrations {
		for i := range list {
			if list[i].IntegrationCapabilityID == id {
				return &list[i], nil
			}
		}
	}
	return nil, domain.ErrIntegrationCapabilityNotFound
}

func (s *stubStore) UpdateIntegrationHealth(_ context.Context, id, status string, claim domain.IdempotencyClaim) error {
	return s.withIdempotency(claim, func() error {
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
	})
}

func (s *stubStore) CreateRelease(_ context.Context, r *domain.Release, claim domain.IdempotencyClaim) error {
	return s.withIdempotency(claim, func() error {
		s.releases[r.CapabilityID] = append(s.releases[r.CapabilityID], *r)
		return nil
	})
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

func (s *stubStore) GetReleaseByID(_ context.Context, id string) (*domain.Release, error) {
	for _, list := range s.releases {
		for i := range list {
			if list[i].ReleaseID == id {
				return &list[i], nil
			}
		}
	}
	return nil, domain.ErrReleaseNotFound
}

func (s *stubStore) CreateCapabilityClaim(_ context.Context, c *domain.CapabilityClaim, claim domain.IdempotencyClaim) error {
	return s.withIdempotency(claim, func() error {
		if _, ok := s.capabilities[c.CapabilityID]; !ok {
			return domain.ErrCapabilityNotFound
		}
		s.claims[c.CapabilityID] = append(s.claims[c.CapabilityID], *c)
		return nil
	})
}

func (s *stubStore) ListClaimsByCapability(_ context.Context, capabilityID string) ([]domain.CapabilityClaim, error) {
	return s.claims[capabilityID], nil
}

func (s *stubStore) GetCapabilityClaimByID(_ context.Context, id string) (*domain.CapabilityClaim, error) {
	for _, list := range s.claims {
		for i := range list {
			if list[i].ClaimID == id {
				return &list[i], nil
			}
		}
	}
	return nil, domain.ErrClaimNotFound
}

// ResolveCapability mirrors pg_store.go's fail-closed logic exactly (2026-10-05
// re-audit, Gap 1): an unrecognized release/market-release state, or an
// integration health status that isn't one of the 4 defined values, must
// never be silently treated as the enabled/allowed path.
func (s *stubStore) ResolveCapability(ctx context.Context, capabilityCode, marketCode string) (*domain.CapabilityResolution, error) {
	cap, err := s.GetCapabilityByCode(ctx, capabilityCode)
	if err != nil {
		return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "CAPABILITY_UNKNOWN"}, nil
	}
	if release, err := s.GetCurrentRelease(ctx, cap.CapabilityID); err == nil {
		switch release.State {
		case domain.ReleaseStateDisabled:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "DISABLED"}, nil
		case domain.ReleaseStateIncidentRestricted:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "INCIDENT_RESTRICTED"}, nil
		case domain.ReleaseStateGA, domain.ReleaseStateBeta, domain.ReleaseStatePilot, domain.ReleaseStateInternal:
			// Recognized, non-blocking — fall through.
		default:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "DISABLED", Detail: "unrecognized release state: " + string(release.State)}, nil
		}
	}
	if marketCode != "" {
		m, err := s.GetActiveMarketRelease(ctx, cap.CapabilityID, marketCode)
		if err != nil {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED"}, nil
		}
		switch m.State {
		case domain.MarketReleaseRestricted, domain.MarketReleaseSuspended, domain.MarketReleaseRetired:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED"}, nil
		case domain.MarketReleaseInternal, domain.MarketReleasePilot, domain.MarketReleaseBeta, domain.MarketReleaseGA:
			// Recognized, non-blocking — fall through.
		default:
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "MARKET_BLOCKED", Detail: "unrecognized market release state: " + string(m.State)}, nil
		}
	}
	for _, integ := range s.integrations[cap.CapabilityID] {
		if !integ.Certified || !domain.IntegrationHealthStatus(integ.HealthStatus).Valid() || integ.HealthStatus == string(domain.HealthStatusFailed) {
			return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: false, ReasonCode: "PROVIDER_UNAVAILABLE"}, nil
		}
	}
	return &domain.CapabilityResolution{CapabilityCode: capabilityCode, Enabled: true, ReasonCode: "ENABLED"}, nil
}

type stubAuthz struct{ err error }

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

var _ AuthzChecker = (*stubAuthz)(nil)

var testRequestSequence atomic.Uint64

func newTestHandler() *Handler {
	logger, _ := zap.NewDevelopment()
	return New(newStubStore(), &stubAuthz{}, logger)
}

// newTestHandlerWithStore returns the test store so event-outbox records can
// be inspected without simulating Kafka publication inside the HTTP handler.
func newTestHandlerWithPublisher() (*Handler, *stubStore) {
	logger, _ := zap.NewDevelopment()
	st := newStubStore()
	return New(st, &stubAuthz{}, logger), st
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
	r.Header.Set("X-Tenant-Id", "tenant-test-01")
	r.Header.Set("Idempotency-Key", "test-key-"+strconv.FormatUint(testRequestSequence.Add(1), 10))
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

func TestCreateCapabilityClaim_SoDDistinctPrincipalsEnforced(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "PAYMENT_ROUTING")

	// Negative Case: wording_owner == approved_by (SoD violation) -> Expect 400 Bad Request.
	wSame := httptest.NewRecorder()
	r.ServeHTTP(wSame, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/claims", domain.CreateCapabilityClaimRequest{
		ClaimText:               "Instant settlement capability across EEA markets.",
		WordingOwnerPrincipalID: "principal-user-01",
		ApprovedByPrincipalID:   "principal-user-01",
	}))
	if wSame.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when wording owner equals approver, got %d: %s", wSame.Code, wSame.Body.String())
	}
	if !bytes.Contains(wSame.Body.Bytes(), []byte("must be distinct principals")) {
		t.Errorf("expected error message to mention distinct principals, got %s", wSame.Body.String())
	}

	// Positive Case: wording_owner != approved_by (SoD satisfied) -> Expect 201 Created.
	wDistinct := httptest.NewRecorder()
	r.ServeHTTP(wDistinct, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/claims", domain.CreateCapabilityClaimRequest{
		ClaimText:               "Instant settlement capability across EEA markets.",
		WordingOwnerPrincipalID: "principal-wording-owner",
		ApprovedByPrincipalID:   "principal-legal-approver",
	}))
	if wDistinct.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created when wording owner and approver are distinct, got %d: %s", wDistinct.Code, wDistinct.Body.String())
	}

	var claim domain.CapabilityClaim
	if err := json.NewDecoder(wDistinct.Body).Decode(&claim); err != nil {
		t.Fatalf("failed to decode created claim: %v", err)
	}
	if claim.WordingOwnerPrincipalID != "principal-wording-owner" || claim.ApprovedByPrincipalID != "principal-legal-approver" {
		t.Errorf("claim contains unexpected principals: %+v", claim)
	}
}

func TestCreateCapabilityClaim_ExpiryReviewDateCurrentBoundaryBehavior(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "CLAIM_REVIEW_DATE")

	for _, test := range []struct {
		name       string
		date       string
		wantStatus int
		wantDate   string
	}{
		{name: "valid RFC3339", date: "2027-01-01T00:00:00Z", wantStatus: http.StatusCreated, wantDate: "2027-01-01T00:00:00Z"},
		{name: "malformed", date: "2027-01-01", wantStatus: http.StatusBadRequest},
		{name: "omitted remains optional pending contract clarification", wantStatus: http.StatusCreated},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/claims", domain.CreateCapabilityClaimRequest{
				ClaimText:               "Review date boundary test",
				WordingOwnerPrincipalID: "principal-wording-owner",
				ApprovedByPrincipalID:   "principal-legal-approver",
				ExpiryReviewDate:        test.date,
			}))
			if w.Code != test.wantStatus {
				t.Fatalf("expected %d for expiry_review_date %q, got %d: %s",
					test.wantStatus, test.date, w.Code, w.Body.String())
			}
			if test.wantStatus != http.StatusCreated {
				return
			}
			var claim domain.CapabilityClaim
			if err := json.NewDecoder(w.Body).Decode(&claim); err != nil {
				t.Fatalf("decode created claim: %v", err)
			}
			if test.wantDate == "" && claim.ExpiryReviewDate != nil {
				t.Fatalf("omitted date should remain nil under current behavior, got %v", claim.ExpiryReviewDate)
			}
			if test.wantDate != "" && (claim.ExpiryReviewDate == nil || claim.ExpiryReviewDate.Format(time.RFC3339) != test.wantDate) {
				t.Fatalf("valid RFC3339 date was not preserved: %v", claim.ExpiryReviewDate)
			}
		})
	}
}

// ── Gap 1: enum validation (2026-10-05 re-audit) ─────────────────────────────

func TestCreateMarketRelease_InvalidState_Rejected(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "ENUM_MARKET_RELEASE")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/market-releases", domain.CreateMarketReleaseRequest{
		MarketCode: "US", LegalApprovalStatus: "APPROVED", State: "GAA_TYPO", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid state, got %d: %s", w.Code, w.Body.String())
	}

	// Valid state must still work (do not change valid existing behavior).
	wValid := httptest.NewRecorder()
	r.ServeHTTP(wValid, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/market-releases", domain.CreateMarketReleaseRequest{
		MarketCode: "US", LegalApprovalStatus: "APPROVED", State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if wValid.Code != http.StatusCreated {
		t.Fatalf("expected 201 for valid state GA, got %d: %s", wValid.Code, wValid.Body.String())
	}
}

func TestCreateCapability_ExecutionRiskClassValidation(t *testing.T) {
	validValues := []string{"LOW", "MEDIUM", "HIGH", "CRITICAL"}
	for _, value := range validValues {
		t.Run(value, func(t *testing.T) {
			h := newTestHandler()
			r := newTestRouter(h)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
				CapabilityCode: "RISK_" + value, ModuleDomain: "TEST", ExecutionRiskClass: value,
			}))
			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201 for documented risk class %s, got %d: %s", value, w.Code, w.Body.String())
			}
		})
	}

	for _, value := range []string{"", "UNCLASSIFIED"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			h := newTestHandler()
			r := newTestRouter(h)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
				CapabilityCode: "RISK_INVALID", ModuleDomain: "TEST", ExecutionRiskClass: value,
			}))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for missing/invalid execution_risk_class %q, got %d: %s", value, w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateMarketRelease_LegalApprovalStatusValidation(t *testing.T) {
	validValues := []string{"APPROVED", "PENDING", "REJECTED"}
	for _, value := range validValues {
		t.Run(value, func(t *testing.T) {
			h := newTestHandler()
			r := newTestRouter(h)
			cap := createTestCapability(t, r, "LEGAL_"+value)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/market-releases", domain.CreateMarketReleaseRequest{
				MarketCode: "US", LegalApprovalStatus: value, State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
			}))
			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201 for documented legal status %s, got %d: %s", value, w.Code, w.Body.String())
			}
		})
	}

	for _, value := range []string{"", "IN_REVIEW"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			h := newTestHandler()
			r := newTestRouter(h)
			cap := createTestCapability(t, r, "LEGAL_INVALID")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/market-releases", domain.CreateMarketReleaseRequest{
				MarketCode: "US", LegalApprovalStatus: value, State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
			}))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400 for missing/invalid legal_approval_status %q, got %d: %s", value, w.Code, w.Body.String())
			}
		})
	}
}

func TestSetReleaseState_InvalidState_Rejected(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "ENUM_RELEASE_STATE")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/release-state", domain.SetReleaseStateRequest{
		State: "NOT_A_REAL_STATE",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid release state, got %d: %s", w.Code, w.Body.String())
	}

	wValid := httptest.NewRecorder()
	r.ServeHTTP(wValid, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/release-state", domain.SetReleaseStateRequest{
		State: "GA",
	}))
	if wValid.Code != http.StatusCreated {
		t.Fatalf("expected 201 for valid release state GA, got %d: %s", wValid.Code, wValid.Body.String())
	}
}

func TestCreateIntegrationCapability_InvalidHealthStatus_Rejected(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "ENUM_INTEGRATION_CREATE")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "stripe", HealthStatus: "FAILD",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid health_status, got %d: %s", w.Code, w.Body.String())
	}

	// Omitted health_status must still default through (do not change valid existing behavior).
	wOmitted := httptest.NewRecorder()
	r.ServeHTTP(wOmitted, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "adyen",
	}))
	if wOmitted.Code != http.StatusCreated {
		t.Fatalf("expected 201 when health_status is omitted, got %d: %s", wOmitted.Code, wOmitted.Body.String())
	}
}

func TestUpdateIntegrationHealth_InvalidHealthStatus_Rejected(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "ENUM_HEALTH_UPDATE")
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "plaid", HealthStatus: "HEALTHY",
	}))
	var integ domain.IntegrationCapability
	_ = json.NewDecoder(wCreate.Body).Decode(&integ)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPut, "/v1/integration-capabilities/"+integ.IntegrationCapabilityID+"/health", domain.UpdateIntegrationHealthRequest{
		HealthStatus: "KINDA_OK",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid health_status, got %d: %s", w.Code, w.Body.String())
	}

	wValid := httptest.NewRecorder()
	r.ServeHTTP(wValid, buildRequest(http.MethodPut, "/v1/integration-capabilities/"+integ.IntegrationCapabilityID+"/health", domain.UpdateIntegrationHealthRequest{
		HealthStatus: "DEGRADED",
	}))
	if wValid.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid health_status DEGRADED, got %d: %s", wValid.Code, wValid.Body.String())
	}
}

// TestResolveCapability_FailsClosedOnCorruptedState proves the store-layer
// defense-in-depth added alongside the handler validation above: even a row
// that pre-dates that validation (an unrecognized state value already
// persisted) must resolve to blocked, never silently to enabled.
func TestResolveCapability_FailsClosedOnCorruptedState(t *testing.T) {
	st := newStubStore()
	logger, _ := zap.NewDevelopment()
	h := New(st, &stubAuthz{}, logger)
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "CORRUPTED_RELEASE_STATE")

	// Bypass the handler's new validation to simulate a pre-existing corrupted
	// row (e.g. written before this gate existed), directly via the store.
	if err := st.CreateRelease(context.Background(), &domain.Release{
		ReleaseID: "rel-corrupt-1", CapabilityID: cap.CapabilityID,
		State: domain.ReleaseState("CORRUPTED_VALUE"),
	}, domain.IdempotencyClaim{
		TenantID: "tenant-test-01", Operation: "seed-corrupted-release",
		PrincipalID: "principal-test-01", Key: "seed-corrupted-release",
		RequestSHA256: "seed", ResourceID: "rel-corrupt-1",
		ResponseStatus: http.StatusCreated, ResponseBody: []byte(`{}`),
	}); err != nil {
		t.Fatalf("failed to seed corrupted release: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capability-resolution/CORRUPTED_RELEASE_STATE", nil))
	var resolved domain.CapabilityResolution
	_ = json.NewDecoder(w.Body).Decode(&resolved)
	if resolved.Enabled {
		t.Fatalf("expected an unrecognized release state to fail closed (Enabled=false), got %+v", resolved)
	}
}

// ── Gap 2: authorization on read endpoints (2026-10-05 re-audit) ────────────

func rawRequest(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

func TestGetCapability_NoPrincipal_Returns401(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AUTH_GET_CAPABILITY")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawRequest(http.MethodGet, "/v1/capabilities/"+cap.CapabilityID))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetCapability_AuthzDenied_Returns403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capabilities/anything", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authz denies, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetCapability_Authorized_Succeeds(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AUTH_GET_CAPABILITY_OK")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capabilities/"+cap.CapabilityID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authorized, existing capability, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListIntegrationCapabilities_NoPrincipal_Returns401(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AUTH_LIST_INTEG")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawRequest(http.MethodGet, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListIntegrationCapabilities_Authorized_Succeeds(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AUTH_LIST_INTEG_OK")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authorized request, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListCapabilityClaims_NoPrincipal_Returns401(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AUTH_LIST_CLAIMS")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawRequest(http.MethodGet, "/v1/capabilities/"+cap.CapabilityID+"/claims"))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Principal-Id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListCapabilityClaims_AuthzDenied_Returns403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(newStubStore(), &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capabilities/anything/claims", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when authz denies, got %d: %s", w.Code, w.Body.String())
	}
}

func TestListCapabilityClaims_Authorized_Succeeds(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "AUTH_LIST_CLAIMS_OK")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/capabilities/"+cap.CapabilityID+"/claims", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for an authorized request, got %d: %s", w.Code, w.Body.String())
	}
}

// TestResolveCapability_AuthorizationNotWeakened proves ResolveCapability
// still requires no X-Principal-Id at all — its documented, deliberate
// open-read posture must not have been touched by the Gap 2 fix above.
func TestResolveCapability_AuthorizationNotWeakened(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	createTestCapability(t, r, "RESOLVE_STILL_PUBLIC")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, rawRequest(http.MethodGet, "/v1/capability-resolution/RESOLVE_STILL_PUBLIC"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected ResolveCapability to remain accessible with no X-Principal-Id, got %d: %s", w.Code, w.Body.String())
	}
}

// ── Gap 3: Kafka events on all 6 writes (2026-10-05 re-audit) ───────────────

func TestCreateMarketRelease_PublishesEvent(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "EVENT_MARKET_RELEASE")
	pub.published = nil // reset after capability.created from the helper above

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/market-releases", domain.CreateMarketReleaseRequest{
		MarketCode: "GB", LegalApprovalStatus: "APPROVED", State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected exactly 1 event published, got %d", len(pub.published))
	}
	if pub.published[0].EventType != "market_release.created" {
		t.Errorf("expected event type market_release.created, got %s", pub.published[0].EventType)
	}
	if pub.published[0].EntityID != cap.CapabilityID {
		t.Errorf("expected entity ID %s, got %s", cap.CapabilityID, pub.published[0].EntityID)
	}
}

func TestCreateIntegrationCapability_PublishesEvent(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "EVENT_INTEGRATION_CREATE")
	pub.published = nil // reset after capability.created from the helper above

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "stripe", HealthStatus: "HEALTHY",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(pub.published) != 1 || pub.published[0].EventType != "integration_capability.created" {
		t.Fatalf("expected exactly 1 integration_capability.created event, got %+v", pub.published)
	}
}

func TestUpdateIntegrationHealth_PublishesEvent(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "EVENT_HEALTH_UPDATE")
	wCreate := httptest.NewRecorder()
	r.ServeHTTP(wCreate, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "adyen", HealthStatus: "HEALTHY",
	}))
	var integ domain.IntegrationCapability
	_ = json.NewDecoder(wCreate.Body).Decode(&integ)
	pub.published = nil // reset after the create event above, to isolate the health-update event

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPut, "/v1/integration-capabilities/"+integ.IntegrationCapabilityID+"/health", domain.UpdateIntegrationHealthRequest{
		HealthStatus: "DEGRADED",
	}))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if len(pub.published) != 1 || pub.published[0].EventType != "integration_capability.health_updated" {
		t.Fatalf("expected exactly 1 integration_capability.health_updated event, got %+v", pub.published)
	}
}

func TestCreateCapabilityClaim_PublishesEvent(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "EVENT_CLAIM_CREATE")
	pub.published = nil // reset after capability.created from the helper above

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/claims", domain.CreateCapabilityClaimRequest{
		ClaimText: "Claim for event test.", WordingOwnerPrincipalID: "owner-1", ApprovedByPrincipalID: "approver-1",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if len(pub.published) != 1 || pub.published[0].EventType != "capability_claim.created" {
		t.Fatalf("expected exactly 1 capability_claim.created event, got %+v", pub.published)
	}
}

// TestCreateMarketRelease_FailedPersistence_NoEventPublished proves a failed
// write (here: a nonexistent capability_id, see Gap 4 below) never emits a
// success event.
func TestCreateMarketRelease_FailedPersistence_NoEventPublished(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/does-not-exist/market-releases", domain.CreateMarketReleaseRequest{
		MarketCode: "US", LegalApprovalStatus: "APPROVED", State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent capability_id, got %d: %s", w.Code, w.Body.String())
	}
	if len(pub.published) != 0 {
		t.Fatalf("expected no event published on failed persistence, got %+v", pub.published)
	}
}

// ── Gap 4: foreign-key violation -> 404, not 500 (2026-10-05 re-audit) ──────

func TestCreateMarketRelease_InvalidCapabilityID_Returns404(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/does-not-exist/market-releases", domain.CreateMarketReleaseRequest{
		MarketCode: "US", LegalApprovalStatus: "APPROVED", State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent capability_id, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("SQLSTATE")) || bytes.Contains(w.Body.Bytes(), []byte("pgconn")) {
		t.Fatalf("response must not expose raw database error details: %s", w.Body.String())
	}
}

func TestCreateIntegrationCapability_InvalidCapabilityID_Returns404(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/does-not-exist/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
		ProviderCode: "stripe",
	}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent capability_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateCapabilityClaim_InvalidCapabilityID_Returns404(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/capabilities/does-not-exist/claims", domain.CreateCapabilityClaimRequest{
		ClaimText: "x", WordingOwnerPrincipalID: "a", ApprovedByPrincipalID: "b",
	}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent capability_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateCapability_IdempotencyReplayReturnsOriginalAndDoesNotRepublish(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	st := h.store.(*stubStore)
	r := newTestRouter(h)
	body := domain.CreateCapabilityRequest{
		CapabilityCode: "IDEMPOTENT_CAPABILITY", ModuleDomain: "billing", ExecutionRiskClass: "LOW",
	}
	request := func() *http.Request {
		req := buildRequest(http.MethodPost, "/v1/capabilities", body)
		req.Header.Set("Idempotency-Key", "replay-key")
		return req
	}

	first := httptest.NewRecorder()
	r.ServeHTTP(first, request())
	replay := httptest.NewRecorder()
	r.ServeHTTP(replay, request())

	if first.Code != http.StatusCreated || replay.Code != first.Code {
		t.Fatalf("expected first and replay status 201, got %d and %d", first.Code, replay.Code)
	}
	if replay.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("expected replay marker, got headers %v", replay.Header())
	}
	var firstResult, replayResult domain.Capability
	if err := json.Unmarshal(first.Body.Bytes(), &firstResult); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if err := json.Unmarshal(replay.Body.Bytes(), &replayResult); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if firstResult.CapabilityID != replayResult.CapabilityID {
		t.Fatalf("replay returned a different capability: first=%s replay=%s", firstResult.CapabilityID, replayResult.CapabilityID)
	}
	if len(st.capabilities) != 1 {
		t.Fatalf("expected one persisted capability, got %d", len(st.capabilities))
	}
	if len(pub.published) != 1 || pub.published[0].EventType != "capability.created" {
		t.Fatalf("expected exactly one capability.created event across original and replay, got %+v", pub.published)
	}
}

func TestAllWrites_IdempotencyReplayPreservesResponseAndEmitsOneEvent(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	r := newTestRouter(h)

	performReplay := func(method, path, key string, body interface{}, wantStatus int) []byte {
		t.Helper()
		makeRequest := func() *http.Request {
			req := buildRequest(method, path, body)
			req.Header.Set("Idempotency-Key", key)
			return req
		}
		first := httptest.NewRecorder()
		r.ServeHTTP(first, makeRequest())
		replay := httptest.NewRecorder()
		r.ServeHTTP(replay, makeRequest())
		if first.Code != wantStatus || replay.Code != wantStatus {
			t.Fatalf("%s %s: expected status %d for original and replay, got %d and %d: %s",
				method, path, wantStatus, first.Code, replay.Code, replay.Body.String())
		}
		if replay.Header().Get("Idempotent-Replayed") != "true" {
			t.Fatalf("%s %s: expected replay marker", method, path)
		}
		var firstJSON, replayJSON interface{}
		if err := json.Unmarshal(first.Body.Bytes(), &firstJSON); err != nil {
			t.Fatalf("decode original response for %s %s: %v", method, path, err)
		}
		if err := json.Unmarshal(replay.Body.Bytes(), &replayJSON); err != nil {
			t.Fatalf("decode replay response for %s %s: %v", method, path, err)
		}
		firstBody, _ := json.Marshal(firstJSON)
		replayBody, _ := json.Marshal(replayJSON)
		if !bytes.Equal(firstBody, replayBody) {
			t.Fatalf("%s %s: replay body differs: first=%s replay=%s", method, path, firstBody, replayBody)
		}
		return first.Body.Bytes()
	}

	var capability domain.Capability
	_ = json.Unmarshal(performReplay(http.MethodPost, "/v1/capabilities", "all-capability-key",
		domain.CreateCapabilityRequest{
			CapabilityCode: "ALL_WRITE_REPLAY", ModuleDomain: "billing", ExecutionRiskClass: "LOW",
		}, http.StatusCreated), &capability)
	performReplay(http.MethodPost, "/v1/capabilities/"+capability.CapabilityID+"/market-releases", "all-market-key",
		domain.CreateMarketReleaseRequest{
			MarketCode: "GB", LegalApprovalStatus: "APPROVED", State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
		}, http.StatusCreated)

	var integration domain.IntegrationCapability
	_ = json.Unmarshal(performReplay(http.MethodPost,
		"/v1/capabilities/"+capability.CapabilityID+"/integration-capabilities", "all-integration-key",
		domain.CreateIntegrationCapabilityRequest{ProviderCode: "replay-provider", HealthStatus: "HEALTHY"},
		http.StatusCreated), &integration)
	performReplay(http.MethodPut, "/v1/integration-capabilities/"+integration.IntegrationCapabilityID+"/health",
		"all-health-key", domain.UpdateIntegrationHealthRequest{HealthStatus: "DEGRADED"}, http.StatusOK)
	performReplay(http.MethodPost, "/v1/capabilities/"+capability.CapabilityID+"/release-state", "all-release-key",
		domain.SetReleaseStateRequest{State: "GA", Reason: "replay test"}, http.StatusCreated)
	performReplay(http.MethodPost, "/v1/capabilities/"+capability.CapabilityID+"/claims", "all-claim-key",
		domain.CreateCapabilityClaimRequest{
			ClaimText: "Replay-safe claim.", WordingOwnerPrincipalID: "owner", ApprovedByPrincipalID: "checker",
		}, http.StatusCreated)

	wantEvents := map[string]int{
		"capability.created":                    1,
		"market_release.created":                1,
		"integration_capability.created":        1,
		"integration_capability.health_updated": 1,
		"capability_release.state_changed":      1,
		"capability_claim.created":              1,
	}
	gotEvents := make(map[string]int)
	for _, event := range pub.published {
		gotEvents[event.EventType]++
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) {
		t.Fatalf("expected exactly one of each existing write event across first writes and replays: got %v", gotEvents)
	}
}

func TestCreateCapability_IdempotencyKeyReuseWithDifferentBodyConflicts(t *testing.T) {
	h := newTestHandler()
	st := h.store.(*stubStore)
	r := newTestRouter(h)
	request := func(code string) *http.Request {
		req := buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
			CapabilityCode: code, ModuleDomain: "billing", ExecutionRiskClass: "LOW",
		})
		req.Header.Set("Idempotency-Key", "reused-key")
		return req
	}

	first := httptest.NewRecorder()
	r.ServeHTTP(first, request("IDEMPOTENCY_BODY_ONE"))
	conflict := httptest.NewRecorder()
	r.ServeHTTP(conflict, request("IDEMPOTENCY_BODY_TWO"))

	if first.Code != http.StatusCreated || conflict.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 201 then 422 for changed request, got %d then %d: %s", first.Code, conflict.Code, conflict.Body.String())
	}
	if len(st.capabilities) != 1 {
		t.Fatalf("different body created a second capability: count=%d", len(st.capabilities))
	}
}

func TestCreateCapability_ConcurrentIdempotentRequestsCreateOneResource(t *testing.T) {
	h, pub := newTestHandlerWithPublisher()
	st := h.store.(*stubStore)
	r := newTestRouter(h)
	body := domain.CreateCapabilityRequest{
		CapabilityCode: "IDEMPOTENCY_CONCURRENT", ModuleDomain: "billing", ExecutionRiskClass: "LOW",
	}
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			w := httptest.NewRecorder()
			req := buildRequest(http.MethodPost, "/v1/capabilities", body)
			req.Header.Set("Idempotency-Key", "concurrent-key")
			r.ServeHTTP(w, req)
			results <- w
		}()
	}
	first, second := <-results, <-results
	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("expected both concurrent responses to be 201, got %d and %d", first.Code, second.Code)
	}
	if (first.Header().Get("Idempotent-Replayed") == "true") == (second.Header().Get("Idempotent-Replayed") == "true") {
		t.Fatalf("expected exactly one original response and one replay")
	}
	if len(st.capabilities) != 1 {
		t.Fatalf("concurrent same-key requests created %d capabilities", len(st.capabilities))
	}
	if len(pub.published) != 1 {
		t.Fatalf("expected one Kafka publish for concurrent requests, got %d", len(pub.published))
	}
}

func TestIdempotencyScope_IsolatedByTenantAndOperation(t *testing.T) {
	h := newTestHandler()
	st := h.store.(*stubStore)
	r := newTestRouter(h)
	create := func(tenant, code string) *httptest.ResponseRecorder {
		req := buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
			CapabilityCode: code, ModuleDomain: "billing", ExecutionRiskClass: "LOW",
		})
		req.Header.Set("Idempotency-Key", "shared-key")
		req.Header.Set("X-Tenant-Id", tenant)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := create("tenant-one", "TENANT_ONE_CAPABILITY"); w.Code != http.StatusCreated {
		t.Fatalf("first tenant create failed: %d %s", w.Code, w.Body.String())
	}
	if w := create("tenant-two", "TENANT_TWO_CAPABILITY"); w.Code != http.StatusCreated {
		t.Fatalf("same key in another tenant was not isolated: %d %s", w.Code, w.Body.String())
	}
	if len(st.capabilities) != 2 {
		t.Fatalf("expected one resource per tenant, got %d", len(st.capabilities))
	}

	cap := createTestCapability(t, r, "OPERATION_SCOPE_CAPABILITY")
	releaseRequest := buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/release-state",
		domain.SetReleaseStateRequest{State: "GA"})
	releaseRequest.Header.Set("Idempotency-Key", "shared-key")
	release := httptest.NewRecorder()
	r.ServeHTTP(release, releaseRequest)
	if release.Code != http.StatusCreated {
		t.Fatalf("same key on a different operation was incorrectly treated as replay: %d %s", release.Code, release.Body.String())
	}
}

func TestAllWrites_StillRequireIdempotencyKey(t *testing.T) {
	h := newTestHandler()
	r := newTestRouter(h)
	cap := createTestCapability(t, r, "MISSING_IDEMPOTENCY_KEY")

	createIntegration := httptest.NewRecorder()
	r.ServeHTTP(createIntegration, buildRequest(http.MethodPost,
		"/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities",
		domain.CreateIntegrationCapabilityRequest{ProviderCode: "missing-key-provider", HealthStatus: "HEALTHY"}))
	if createIntegration.Code != http.StatusCreated {
		t.Fatalf("failed to prepare integration capability: %d %s", createIntegration.Code, createIntegration.Body.String())
	}
	var integration domain.IntegrationCapability
	if err := json.Unmarshal(createIntegration.Body.Bytes(), &integration); err != nil {
		t.Fatalf("decode integration response: %v", err)
	}

	requests := []*http.Request{
		buildRequest(http.MethodPost, "/v1/capabilities", domain.CreateCapabilityRequest{
			CapabilityCode: "MISSING_KEY_CAP", ModuleDomain: "billing", ExecutionRiskClass: "LOW",
		}),
		buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/market-releases", domain.CreateMarketReleaseRequest{
			MarketCode: "GB", LegalApprovalStatus: "APPROVED", State: "GA", EffectiveFrom: "2026-01-01T00:00:00Z",
		}),
		buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/integration-capabilities", domain.CreateIntegrationCapabilityRequest{
			ProviderCode: "another-provider", HealthStatus: "HEALTHY",
		}),
		buildRequest(http.MethodPut, "/v1/integration-capabilities/"+integration.IntegrationCapabilityID+"/health", domain.UpdateIntegrationHealthRequest{
			HealthStatus: "DEGRADED",
		}),
		buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/release-state", domain.SetReleaseStateRequest{State: "GA"}),
		buildRequest(http.MethodPost, "/v1/capabilities/"+cap.CapabilityID+"/claims", domain.CreateCapabilityClaimRequest{
			ClaimText: "approved claim", WordingOwnerPrincipalID: "writer", ApprovedByPrincipalID: "checker",
		}),
	}
	for i, req := range requests {
		req.Header.Del("Idempotency-Key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("write %d without Idempotency-Key returned %d, expected 400: %s", i+1, w.Code, w.Body.String())
		}
	}
}
