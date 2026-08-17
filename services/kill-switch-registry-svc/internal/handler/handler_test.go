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

	authzpkg "zoiko.io/kill-switch-registry-svc/internal/authz"
	"zoiko.io/kill-switch-registry-svc/internal/domain"
	"zoiko.io/kill-switch-registry-svc/internal/events"
)

// stubStore is a tiny in-memory re-implementation of the real
// ResolveKillSwitch/ListCurrentStates logic — good enough to exercise the
// handler's own validation and wiring without a real database.
type stubStore struct {
	events []domain.KillSwitchEvent
}

func scopeKey(plane, domainName, providerCode, tenantID *string) string {
	deref := func(s *string) string {
		if s == nil {
			return "<nil>"
		}
		return *s
	}
	return deref(plane) + "|" + deref(domainName) + "|" + deref(providerCode) + "|" + deref(tenantID)
}

func (s *stubStore) AppendEvent(_ context.Context, e *domain.KillSwitchEvent) error {
	s.events = append(s.events, *e)
	return nil
}

func (s *stubStore) LatestEventForScope(_ context.Context, plane, domainName, providerCode, tenantID *string) (*domain.KillSwitchEvent, error) {
	key := scopeKey(plane, domainName, providerCode, tenantID)
	var latest *domain.KillSwitchEvent
	for i := range s.events {
		e := &s.events[i]
		if scopeKey(e.Plane, e.Domain, e.ProviderCode, e.TenantID) == key {
			latest = e
		}
	}
	return latest, nil
}

func specificity(e domain.KillSwitchEvent) int {
	n := 0
	for _, v := range []*string{e.Plane, e.Domain, e.ProviderCode, e.TenantID} {
		if v != nil {
			n++
		}
	}
	return n
}

func compatible(stored, want *string) bool {
	return stored == nil || (want != nil && *stored == *want)
}

func (s *stubStore) ResolveKillSwitch(_ context.Context, plane, domainName, providerCode, tenantID *string) (*domain.KillSwitchResolution, error) {
	// Mirrors the real store's event_seq ordering: iterate in append
	// (insertion) order and always overwrite on a match, so the LAST
	// matching event in the slice wins regardless of wall-clock ties —
	// never compare on CreatedAt directly, same doctrine as the real
	// BIGSERIAL event_seq column existing precisely because created_at
	// alone can tie.
	latestByScope := map[string]domain.KillSwitchEvent{}
	for _, e := range s.events {
		if !compatible(e.Plane, plane) || !compatible(e.Domain, domainName) || !compatible(e.ProviderCode, providerCode) || !compatible(e.TenantID, tenantID) {
			continue
		}
		key := scopeKey(e.Plane, e.Domain, e.ProviderCode, e.TenantID)
		latestByScope[key] = e
	}
	var best *domain.KillSwitchEvent
	for key := range latestByScope {
		e := latestByScope[key]
		if e.Action != domain.KillSwitchActionEngage {
			continue
		}
		if best == nil || specificity(e) > specificity(*best) {
			best = &e
		}
	}
	if best == nil {
		return &domain.KillSwitchResolution{Blocked: false}, nil
	}
	return &domain.KillSwitchResolution{Blocked: true, MatchedEvent: best}, nil
}

func (s *stubStore) ListCurrentStates(_ context.Context) ([]domain.KillSwitchState, error) {
	latestByScope := map[string]domain.KillSwitchEvent{}
	for _, e := range s.events {
		key := scopeKey(e.Plane, e.Domain, e.ProviderCode, e.TenantID)
		latestByScope[key] = e
	}
	var out []domain.KillSwitchState
	for _, e := range latestByScope {
		out = append(out, domain.KillSwitchState{
			Plane: e.Plane, Domain: e.Domain, ProviderCode: e.ProviderCode, TenantID: e.TenantID,
			Action: e.Action, Reason: e.Reason, LatestEventAt: e.CreatedAt,
		})
	}
	return out, nil
}

func (s *stubStore) ListHistoryForScope(_ context.Context, plane, domainName, providerCode, tenantID *string) ([]domain.KillSwitchEvent, error) {
	key := scopeKey(plane, domainName, providerCode, tenantID)
	var out []domain.KillSwitchEvent
	for _, e := range s.events {
		if scopeKey(e.Plane, e.Domain, e.ProviderCode, e.TenantID) == key {
			out = append(out, e)
		}
	}
	return out, nil
}

type stubPublisher struct{ calls int }

func (p *stubPublisher) Publish(_ context.Context, _, _, _ string, _ interface{}) error {
	p.calls++
	return nil
}

var _ events.Publisher = (*stubPublisher)(nil)

type stubAuthz struct{ err error }

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

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
	req.Header.Set("X-Principal-Id", "incident-commander-1")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestEngage_RequiresReconciliationProcedureRef(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/kill-switches/engage", domain.EngageKillSwitchRequest{
		Domain:                "AUTOMATION_ACTION",
		Reason:                "runaway automation loop detected",
		ApprovedByPrincipalID: "incident-commander-1",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 missing reconciliation_procedure_ref, got %d — %s", w.Code, w.Body.String())
	}
}

func TestEngage_PlatformWideThenResolveBlocksEverything(t *testing.T) {
	h, _, pub := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/kill-switches/engage", domain.EngageKillSwitchRequest{
		Reason:                     "platform-wide incident INC-1234",
		ReconciliationProcedureRef: "runbook:INC-1234",
		ApprovedByPrincipalID:      "incident-commander-1",
	}))
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d — %s", w.Code, w.Body.String())
	}
	if pub.calls != 1 {
		t.Errorf("expected kill_switch.engaged published once, got %d", pub.calls)
	}

	// A completely unrelated, narrowly-scoped resolve request must still be
	// blocked by the platform-wide switch — that's the whole point of a
	// nil dimension matching everything.
	wResolve := httptest.NewRecorder()
	r.ServeHTTP(wResolve, buildRequest(http.MethodGet, "/v1/kill-switches/resolve?domain=IMPORT_SYNC&tenant_id=t-1", nil))
	var resolution domain.KillSwitchResolution
	_ = json.Unmarshal(wResolve.Body.Bytes(), &resolution)
	if !resolution.Blocked {
		t.Fatalf("expected the platform-wide switch to block an unrelated scope, got %+v", resolution)
	}
}

func TestResolve_MostSpecificEngagedSwitchWins(t *testing.T) {
	h, st, _ := newTestHandler()
	r := newTestRouter(h)

	// Engage a broad domain-only switch for AUTOMATION_ACTION...
	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/kill-switches/engage", domain.EngageKillSwitchRequest{
		Domain:                     "AUTOMATION_ACTION",
		Reason:                     "broad automation concern",
		ReconciliationProcedureRef: "runbook:broad",
		ApprovedByPrincipalID:      "incident-commander-1",
	}))
	// ...then disengage it for one specific tenant only (more specific).
	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/kill-switches/engage", domain.EngageKillSwitchRequest{
		Domain:                     "AUTOMATION_ACTION",
		TenantID:                   "tenant-safe",
		Reason:                     "tenant-safe cleared for automation resumption",
		ReconciliationProcedureRef: "runbook:tenant-safe",
		ApprovedByPrincipalID:      "incident-commander-1",
	}))

	if len(st.events) != 2 {
		t.Fatalf("expected 2 stored events, got %d", len(st.events))
	}

	// tenant-safe should resolve to whichever is most specific — since both
	// are ENGAGE here, the tenant-scoped one (more specific) wins.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/kill-switches/resolve?domain=AUTOMATION_ACTION&tenant_id=tenant-safe", nil))
	var resolution domain.KillSwitchResolution
	_ = json.Unmarshal(w.Body.Bytes(), &resolution)
	if !resolution.Blocked || resolution.MatchedEvent == nil || resolution.MatchedEvent.TenantID == nil || *resolution.MatchedEvent.TenantID != "tenant-safe" {
		t.Fatalf("expected the tenant-specific switch to be the matched event, got %+v", resolution)
	}
}

func TestDisengage_RejectsWhenNotCurrentlyEngaged(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/kill-switches/disengage", domain.DisengageKillSwitchRequest{
		Domain:                "COMMERCIAL_CHARGING",
		Reason:                "attempting to clear a switch that was never engaged",
		ApprovedByPrincipalID: "incident-commander-1",
	}))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 disengaging a never-engaged scope, got %d — %s", w.Code, w.Body.String())
	}
}

func TestEngageThenDisengage_ResolveNoLongerBlocked(t *testing.T) {
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	r.ServeHTTP(httptest.NewRecorder(), buildRequest(http.MethodPost, "/v1/kill-switches/engage", domain.EngageKillSwitchRequest{
		Domain:                     "PUBLIC_CLAIM_PUBLICATION",
		Reason:                     "pending legal review",
		ReconciliationProcedureRef: "runbook:legal-review-" + uuid.NewString(),
		ApprovedByPrincipalID:      "incident-commander-1",
	}))

	wDisengage := httptest.NewRecorder()
	r.ServeHTTP(wDisengage, buildRequest(http.MethodPost, "/v1/kill-switches/disengage", domain.DisengageKillSwitchRequest{
		Domain:                "PUBLIC_CLAIM_PUBLICATION",
		Reason:                "legal review complete, cleared to resume",
		ApprovedByPrincipalID: "incident-commander-1",
	}))
	if wDisengage.Code != http.StatusOK {
		t.Fatalf("expected 200 disengaging an engaged switch, got %d — %s", wDisengage.Code, wDisengage.Body.String())
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodGet, "/v1/kill-switches/resolve?domain=PUBLIC_CLAIM_PUBLICATION", nil))
	var resolution domain.KillSwitchResolution
	_ = json.Unmarshal(w.Body.Bytes(), &resolution)
	if resolution.Blocked {
		t.Fatalf("expected not blocked after disengage, got %+v", resolution)
	}
}

func TestAuthorizationDenied_Engage403(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	h := New(&stubStore{}, &stubPublisher{}, &stubAuthz{err: authzpkg.ErrAuthorizationDenied}, logger)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequest(http.MethodPost, "/v1/kill-switches/engage", domain.EngageKillSwitchRequest{
		Domain:                     "MODEL_PROVIDER_USE",
		Reason:                     "should be denied",
		ReconciliationProcedureRef: "runbook:denied",
		ApprovedByPrincipalID:      "incident-commander-1",
	}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d — %s", w.Code, w.Body.String())
	}
}
