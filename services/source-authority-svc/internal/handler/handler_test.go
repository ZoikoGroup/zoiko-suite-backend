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

	authzpkg "zoiko.io/source-authority-svc/internal/authz"
	"zoiko.io/source-authority-svc/internal/domain"
	"zoiko.io/source-authority-svc/internal/events"
)

type stubStore struct {
	maps  []domain.SourceAuthorityMap
	facts []domain.NormalizedFact
}

func (s *stubStore) CreateSourceAuthorityMap(_ context.Context, m *domain.SourceAuthorityMap) error {
	for _, existing := range s.maps {
		if existing.FieldFamily == m.FieldFamily && existing.SourceSystem == m.SourceSystem && existing.EffectiveFrom.Equal(m.EffectiveFrom) {
			return domain.ErrConflict
		}
	}
	s.maps = append(s.maps, *m)
	return nil
}

func (s *stubStore) ListSourceAuthorityMaps(_ context.Context, fieldFamily string) ([]domain.SourceAuthorityMap, error) {
	var out []domain.SourceAuthorityMap
	for _, m := range s.maps {
		if m.FieldFamily == fieldFamily {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *stubStore) RecordFact(_ context.Context, f *domain.NormalizedFact) error {
	s.facts = append(s.facts, *f)
	return nil
}

// ResolveAuthoritativeFact mirrors the real PgStore's logic in miniature:
// latest fact per source, ranked by precedence, ambiguous if the top tier
// disagrees. Good enough to exercise the handler without a real database.
func (s *stubStore) ResolveAuthoritativeFact(_ context.Context, fieldFamily, entityRef string) (*domain.FactResolution, error) {
	latestBySource := map[string]domain.NormalizedFact{}
	for _, f := range s.facts {
		if f.FieldFamily != fieldFamily || f.EntityRef != entityRef {
			continue
		}
		if existing, ok := latestBySource[f.SourceSystem]; !ok || f.EffectiveAt.After(existing.EffectiveAt) {
			latestBySource[f.SourceSystem] = f
		}
	}

	type ranked struct {
		fact          domain.NormalizedFact
		rank          int
		conflictRoute string
	}
	var all []ranked
	for source, fact := range latestBySource {
		for _, m := range s.maps {
			if m.FieldFamily == fieldFamily && m.SourceSystem == source {
				all = append(all, ranked{fact: fact, rank: m.PrecedenceRank, conflictRoute: m.ConflictRoute})
				break
			}
		}
	}

	result := &domain.FactResolution{FieldFamily: fieldFamily, EntityRef: entityRef}
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
	req.Header.Set("X-Principal-Id", "data-governance-owner-1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func createMap(t *testing.T, r *chi.Mux, fieldFamily, sourceSystem string, rank int) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily:    fieldFamily,
		SourceSystem:   sourceSystem,
		PrecedenceRank: rank,
		ConflictRoute:  "route to Data Governance",
		EffectiveFrom:  "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 creating map, got %d — %s", w.Code, w.Body.String())
	}
}

func recordFact(t *testing.T, r *chi.Mux, fieldFamily, entityRef, sourceSystem string, value string, effectiveAt string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/normalized-facts", domain.RecordFactRequest{
		FieldFamily:  fieldFamily,
		EntityRef:    entityRef,
		SourceSystem: sourceSystem,
		SourceRecord: "rec-1",
		FactValue:    json.RawMessage(value),
		ObservedAt:   effectiveAt,
		EffectiveAt:  effectiveAt,
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 recording fact, got %d — %s", w.Code, w.Body.String())
	}
}

func TestResolve_NoFactsRecorded(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=HR_EMPLOYMENT_STATUS&entity_ref=emp-1", nil))
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

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=HR_EMPLOYMENT_STATUS&entity_ref=emp-1", nil))
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

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=BILLING_CONTACT_EMAIL&entity_ref=acct-1", nil))
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

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/source-authority/resolve?field_family=TAX_JURISDICTION&entity_ref=entity-1", nil))
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
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily: "X", SourceSystem: "SourceA", PrecedenceRank: 1,
		ConflictRoute: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on duplicate map, got %d — %s", w.Code, w.Body.String())
	}
}

func TestCreateSourceAuthorityMap_AuthorizationDenied403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(&stubStore{}, &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/source-authority-maps", domain.CreateSourceAuthorityMapRequest{
		FieldFamily: "X", SourceSystem: "Y", PrecedenceRank: 1,
		ConflictRoute: "x", EffectiveFrom: "2026-01-01T00:00:00Z",
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
}
