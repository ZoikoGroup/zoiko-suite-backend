package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/source-authority-svc/internal/authz"
	"zoiko.io/source-authority-svc/internal/domain"
	"zoiko.io/source-authority-svc/internal/events"
	svcmiddleware "zoiko.io/source-authority-svc/internal/middleware"
)

const testTenant = "tenant-abc"

type stubStore struct {
	maps  []domain.SourceAuthorityMap
	facts []domain.NormalizedFact
}

func (s *stubStore) CreateSourceAuthorityMap(_ context.Context, m *domain.SourceAuthorityMap) (bool, error) {
	// Idempotency first, exactly as the real store orders it: a replay must not
	// be reported as a uniqueness conflict with itself.
	if m.CorrelationID != nil {
		for _, existing := range s.maps {
			if existing.CorrelationID != nil && *existing.CorrelationID == *m.CorrelationID {
				*m = existing
				return false, nil
			}
		}
	}
	for _, existing := range s.maps {
		if existing.FieldFamily == m.FieldFamily && existing.SourceSystem == m.SourceSystem && existing.EffectiveFrom.Equal(m.EffectiveFrom) {
			return false, domain.ErrConflict
		}
	}
	s.maps = append(s.maps, *m)
	return true, nil
}

func (s *stubStore) GetSourceAuthorityMap(_ context.Context, mapID string) (*domain.SourceAuthorityMap, error) {
	for _, m := range s.maps {
		if m.SourceAuthorityMapID == mapID {
			cp := m
			return &cp, nil
		}
	}
	return nil, domain.ErrMapNotFound
}

// ListSourceAuthorityMaps applies the filter for real, including the
// currently-in-force narrowing. A stub that ignored IncludeSuperseded would
// make the supersession tests pass without the behaviour being present.
func (s *stubStore) ListSourceAuthorityMaps(_ context.Context, f domain.ListSourceAuthorityMapsFilter) ([]domain.SourceAuthorityMap, error) {
	now := time.Now().UTC()
	var out []domain.SourceAuthorityMap
	for _, m := range s.maps {
		if f.FieldFamily != "" && m.FieldFamily != f.FieldFamily {
			continue
		}
		if f.SourceSystem != "" && m.SourceSystem != f.SourceSystem {
			continue
		}
		if !f.IncludeSuperseded && !m.EffectiveAt(now) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PrecedenceRank != out[j].PrecedenceRank {
			return out[i].PrecedenceRank < out[j].PrecedenceRank
		}
		return out[i].SourceAuthorityMapID < out[j].SourceAuthorityMapID
	})
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (s *stubStore) SupersedeSourceAuthorityMap(_ context.Context, mapID string, effectiveTo time.Time, principalID string) (*domain.SourceAuthorityMap, error) {
	for i := range s.maps {
		if s.maps[i].SourceAuthorityMapID != mapID {
			continue
		}
		if s.maps[i].SupersededAt != nil {
			return nil, domain.ErrAlreadySuperseded
		}
		if !effectiveTo.After(s.maps[i].EffectiveFrom) {
			return nil, domain.ErrSupersedeBeforeStart
		}
		now := time.Now().UTC()
		s.maps[i].EffectiveTo = &effectiveTo
		s.maps[i].SupersededAt = &now
		s.maps[i].SupersededByPrincipalID = &principalID
		cp := s.maps[i]
		return &cp, nil
	}
	return nil, domain.ErrMapNotFound
}

func (s *stubStore) RecordFact(_ context.Context, f *domain.NormalizedFact) (bool, error) {
	if f.TenantID == "" {
		return false, domain.ErrTenantMissing
	}
	if f.CorrelationID != nil {
		for _, existing := range s.facts {
			if existing.TenantID == f.TenantID && existing.CorrelationID != nil && *existing.CorrelationID == *f.CorrelationID {
				*f = existing
				return false, nil
			}
		}
	}
	s.facts = append(s.facts, *f)
	return true, nil
}

func (s *stubStore) ListNormalizedFacts(_ context.Context, f domain.ListNormalizedFactsFilter) ([]domain.NormalizedFact, error) {
	if f.TenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	var out []domain.NormalizedFact
	for _, fact := range s.facts {
		if fact.TenantID != f.TenantID {
			continue
		}
		if f.FieldFamily != "" && fact.FieldFamily != f.FieldFamily {
			continue
		}
		if f.EntityRef != "" && fact.EntityRef != f.EntityRef {
			continue
		}
		if f.SourceSystem != "" && fact.SourceSystem != f.SourceSystem {
			continue
		}
		out = append(out, fact)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EffectiveAt.After(out[j].EffectiveAt) })
	if f.Offset > 0 {
		if f.Offset >= len(out) {
			return nil, nil
		}
		out = out[f.Offset:]
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// ResolveAuthoritativeFact mirrors the real PgStore's logic in miniature:
// facts scoped to the tenant, latest fact per source, one currently-effective
// rule per source, ranked by precedence, ambiguous if the top tier disagrees,
// and sources with no rule in force reported rather than dropped.
func (s *stubStore) ResolveAuthoritativeFact(_ context.Context, tenantID, fieldFamily, entityRef string) (*domain.FactResolution, error) {
	if tenantID == "" {
		return nil, domain.ErrTenantMissing
	}
	now := time.Now().UTC()

	latestBySource := map[string]domain.NormalizedFact{}
	for _, f := range s.facts {
		if f.TenantID != tenantID || f.FieldFamily != fieldFamily || f.EntityRef != entityRef {
			continue
		}
		if f.EffectiveAt.After(now) {
			continue
		}
		if existing, ok := latestBySource[f.SourceSystem]; !ok || f.EffectiveAt.After(existing.EffectiveAt) {
			latestBySource[f.SourceSystem] = f
		}
	}

	result := &domain.FactResolution{FieldFamily: fieldFamily, EntityRef: entityRef}

	type ranked struct {
		fact          domain.NormalizedFact
		rank          int
		conflictRoute string
	}
	var all []ranked
	sources := make([]string, 0, len(latestBySource))
	for source := range latestBySource {
		sources = append(sources, source)
	}
	sort.Strings(sources) // deterministic, so assertions mean something

	for _, source := range sources {
		fact := latestBySource[source]
		// One rule per source: the most recently effective one in force.
		var current *domain.SourceAuthorityMap
		for i := range s.maps {
			m := s.maps[i]
			if m.FieldFamily != fieldFamily || m.SourceSystem != source || !m.EffectiveAt(now) {
				continue
			}
			if current == nil || m.EffectiveFrom.After(current.EffectiveFrom) {
				cp := m
				current = &cp
			}
		}
		if current == nil {
			result.UnmappedSources = append(result.UnmappedSources, source)
			continue
		}
		all = append(all, ranked{fact: fact, rank: current.PrecedenceRank, conflictRoute: current.ConflictRoute})
	}
	if len(all) == 0 {
		return result, nil
	}

	topRank := all[0].rank
	for _, r := range all {
		if r.rank < topRank {
			topRank = r.rank
		}
	}
	var topTier []ranked
	for _, r := range all {
		if r.rank == topRank {
			topTier = append(topTier, r)
		}
	}
	allAgree := true
	for _, r := range topTier[1:] {
		if string(r.fact.FactValue) != string(topTier[0].fact.FactValue) {
			allAgree = false
		}
	}
	if allAgree {
		result.AuthoritativeFact = &topTier[0].fact
		return result, nil
	}
	result.Ambiguous = true
	result.ConflictRoute = &topTier[0].conflictRoute
	for _, r := range topTier {
		result.ConflictingFacts = append(result.ConflictingFacts, r.fact)
	}
	return result, nil
}

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _ events.PublishParams) error {
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

// newTestRouter installs the tenant that TenantContext would have read off
// X-Tenant-Id, so the routes under test see the same context they see in
// production.
func newTestRouter(h *Handler) *chi.Mux {
	return newTestRouterTenant(h, testTenant)
}

func newTestRouterTenant(h *Handler, tenantID string) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if tenantID != "" {
				req = req.WithContext(svcmiddleware.WithTenant(req.Context(), tenantID))
			}
			next.ServeHTTP(w, req)
		})
	})
	RegisterRoutes(r, h)
	return r
}

func buildRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Principal-Id", "data-governance-owner-1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func do(r *chi.Mux, req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func createMap(t *testing.T, r *chi.Mux, fieldFamily, sourceSystem string, rank int) domain.SourceAuthorityMap {
	t.Helper()
	w := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily:    fieldFamily,
		SourceSystem:   sourceSystem,
		PrecedenceRank: rank,
		ConflictRoute:  "route to Data Governance",
		EffectiveFrom:  "2026-01-01T00:00:00Z",
		CorrelationID:  uuid.NewString(),
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating map, got %d — %s", w.Code, w.Body.String())
	}
	var m domain.SourceAuthorityMap
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m
}

func recordFact(t *testing.T, r *chi.Mux, fieldFamily, entityRef, sourceSystem string, value string, effectiveAt string) {
	t.Helper()
	w := do(r, buildRequest(http.MethodPost, "/v1/normalized-facts", domain.RecordFactRequest{
		FieldFamily:   fieldFamily,
		EntityRef:     entityRef,
		SourceSystem:  sourceSystem,
		SourceRecord:  "rec-1",
		FactValue:     json.RawMessage(value),
		ObservedAt:    effectiveAt,
		EffectiveAt:   effectiveAt,
		CorrelationID: uuid.NewString(),
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 recording fact, got %d — %s", w.Code, w.Body.String())
	}
}

func TestResolve_NoFactsRecorded(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=HR_EMPLOYMENT_STATUS&entity_ref=emp-1", nil))
	var res domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Ambiguous || res.AuthoritativeFact != nil {
		t.Fatalf("expected an empty resolution when no facts exist, got %+v", res)
	}
}

// TestResolve_HighestPrecedenceSourceWins is the core doctrine: when two
// sources disagree, the one with the better (lower-number) precedence
// rank is the answer.
func TestResolve_HighestPrecedenceSourceWins(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "HR_EMPLOYMENT_STATUS", "Kriton", 1)
	createMap(t, r, "HR_EMPLOYMENT_STATUS", "ZoikoLogia", 2)

	recordFact(t, r, "HR_EMPLOYMENT_STATUS", "emp-1", "ZoikoLogia", `"ACTIVE"`, "2026-01-05T00:00:00Z")
	recordFact(t, r, "HR_EMPLOYMENT_STATUS", "emp-1", "Kriton", `"TERMINATED"`, "2026-01-05T00:00:00Z")

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=HR_EMPLOYMENT_STATUS&entity_ref=emp-1", nil))
	var res domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Ambiguous {
		t.Fatalf("expected a clean resolution (different precedence tiers), got ambiguous: %+v", res)
	}
	if res.AuthoritativeFact == nil || res.AuthoritativeFact.SourceSystem != "Kriton" {
		t.Fatalf("expected Kriton (rank 1) to win over ZoikoLogia (rank 2), got %+v", res.AuthoritativeFact)
	}
}

// TestResolve_SameTierDisagreement_IsAmbiguous is doc7 §D2's core
// requirement: two EQUALLY-ranked sources disagreeing must block, never
// silently pick one.
func TestResolve_SameTierDisagreement_IsAmbiguous(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "BILLING_CONTACT_EMAIL", "ADP", 1)
	createMap(t, r, "BILLING_CONTACT_EMAIL", "NetSuite", 1) // same tier, deliberately

	recordFact(t, r, "BILLING_CONTACT_EMAIL", "acct-1", "ADP", `"finance@acme.com"`, "2026-01-05T00:00:00Z")
	recordFact(t, r, "BILLING_CONTACT_EMAIL", "acct-1", "NetSuite", `"billing@acme.com"`, "2026-01-05T00:00:00Z")

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=BILLING_CONTACT_EMAIL&entity_ref=acct-1", nil))
	var res domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if !res.Ambiguous {
		t.Fatalf("expected ambiguous=true for a same-tier disagreement, got %+v", res)
	}
	if res.AuthoritativeFact != nil {
		t.Fatalf("expected no authoritative fact returned while ambiguous, got %+v", res.AuthoritativeFact)
	}
	if len(res.ConflictingFacts) != 2 {
		t.Fatalf("expected both conflicting facts reported, got %d", len(res.ConflictingFacts))
	}
	if res.ConflictRoute == nil || *res.ConflictRoute == "" {
		t.Errorf("expected a conflict_route to be surfaced for the caller to act on")
	}
}

// TestResolve_SameTierAgreement_IsNotAmbiguous proves agreement at the
// same tier is not treated as a conflict — only disagreement is.
func TestResolve_SameTierAgreement_IsNotAmbiguous(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "TAX_JURISDICTION", "SourceA", 1)
	createMap(t, r, "TAX_JURISDICTION", "SourceB", 1)

	recordFact(t, r, "TAX_JURISDICTION", "entity-1", "SourceA", `"IN-KA"`, "2026-01-05T00:00:00Z")
	recordFact(t, r, "TAX_JURISDICTION", "entity-1", "SourceB", `"IN-KA"`, "2026-01-05T00:00:00Z")

	w := do(r, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=TAX_JURISDICTION&entity_ref=entity-1", nil))
	var res domain.FactResolution
	_ = json.Unmarshal(w.Body.Bytes(), &res)
	if res.Ambiguous {
		t.Fatalf("expected agreement at the same tier to NOT be ambiguous, got %+v", res)
	}
	if res.AuthoritativeFact == nil {
		t.Fatalf("expected an authoritative fact when both same-tier sources agree")
	}
}

func TestCreateSourceAuthorityMap_DuplicateConflict(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	createMap(t, r, "X", "SourceA", 1)
	// A DIFFERENT correlation id: a genuinely new request asserting the same
	// (field_family, source_system, effective_from), which is the real
	// uniqueness conflict rather than a replay.
	w := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily: "X", SourceSystem: "SourceA", PrecedenceRank: 1,
		ConflictRoute: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
		CorrelationID: uuid.NewString(),
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate map, got %d — %s", w.Code, w.Body.String())
	}
}

func TestCreateSourceAuthorityMap_AuthorizationDenied403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(&stubStore{}, &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := do(r, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily: "X", SourceSystem: "Y", PrecedenceRank: 1,
		ConflictRoute: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
		CorrelationID: uuid.NewString(),
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
}
