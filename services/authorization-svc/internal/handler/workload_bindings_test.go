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
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/cache"
	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// Governance audit N-09: workload bindings could not be written,
// allowed_actions was never enforced, and the workload layer applied only to
// callers that declared principal_type WORKLOAD.

type workloadStub struct {
	*stubStore
	bound      map[string]bool
	saved      *domain.WorkloadBinding
	savedEvent domain.OutboxMessage
	setActive  *bool
	missing    bool
}

func (s *workloadStub) SaveWorkloadBinding(_ context.Context, wb domain.WorkloadBinding, ev domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	s.saved, s.savedEvent = &wb, ev
	return &wb, nil
}
func (s *workloadStub) SetWorkloadBindingActive(_ context.Context, id, tenant string, active bool, ev domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	if s.missing {
		return nil, domain.ErrWorkloadBindingNotFound
	}
	s.setActive, s.savedEvent = &active, ev
	return &domain.WorkloadBinding{WorkloadID: id, TenantID: tenant, ActiveFlag: active}, nil
}
func (s *workloadStub) ListWorkloadBindings(context.Context, string) ([]domain.WorkloadBinding, error) {
	return []domain.WorkloadBinding{}, nil
}
func (s *workloadStub) IsBoundWorkload(_ context.Context, principalID string) (bool, error) {
	return s.bound[principalID], nil
}

func workloadRouter(s *workloadStub) chi.Router {
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, &stubValidator{}, siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop()))
	return r
}

func workloadAuthorize(t *testing.T, r chi.Router, principal, action, extra string) (string, string) {
	t.Helper()
	body := `{"principal_id":"` + principal + `","legal_entity_id":"11111111-1111-4111-8111-aaaaaaaaaaa1","action_type":"` + action +
		`","tenant_id":"11111111-1111-4111-8111-111111111111"` + extra + `}`
	req := httptest.NewRequest(http.MethodPost, handler.AuthorizePath, bytes.NewBufferString(body))
	req.Header.Set("X-Audience", "ledger-api")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	return decodeDecision(t, w)
}

func TestWorkload_BindingActionsAreEnforced(t *testing.T) {
	binding := &domain.WorkloadBinding{WorkloadID: "ledger-sync", AllowedAudience: "ledger-api",
		AllowedActions: []string{"GL_JOURNAL_READ"}, ActiveFlag: true}
	s := &workloadStub{stubStore: &stubStore{rbacActions: []string{"GL_JOURNAL_READ", "GL_POST"}, rbacBasis: "rbac:role=R", workloadBinding: binding}}
	r := workloadRouter(s)
	if outcome, basis := workloadAuthorize(t, r, "ledger-sync", "GL_JOURNAL_READ", `,"principal_type":"WORKLOAD"`); outcome != "GRANTED" {
		t.Errorf("bound action: %s (%s), want GRANTED", outcome, basis)
	}
	if outcome, basis := workloadAuthorize(t, r, "ledger-sync", "GL_POST", `,"principal_type":"WORKLOAD"`); outcome != "DENIED" || basis != "workload:action_not_bound" {
		t.Errorf("action outside the binding (RBAC grants it): %s (%s), want DENIED workload:action_not_bound", outcome, basis)
	}
}

// A bound workload that does not declare itself is still evaluated as one.
func TestWorkload_UndeclaredBoundWorkloadIsStillAWorkload(t *testing.T) {
	s := &workloadStub{stubStore: &stubStore{rbacActions: []string{"GL_POST"}, rbacBasis: "rbac:role=R"},
		bound: map[string]bool{"ledger-sync": true}}
	// No binding in THIS tenant: unbound, whatever RBAC holds.
	if outcome, basis := workloadAuthorize(t, workloadRouter(s), "ledger-sync", "GL_POST", ``); outcome != "DENIED" || basis != "workload:tenant_context_unbound" {
		t.Errorf("undeclared bound workload: %s (%s), want DENIED workload:tenant_context_unbound", outcome, basis)
	}
	// A human principal is unaffected.
	if outcome, _ := workloadAuthorize(t, workloadRouter(s), "alice", "GL_POST", ``); outcome != "GRANTED" {
		t.Errorf("human principal: %s, want GRANTED", outcome)
	}
}

func postWorkloadAdmin(r chi.Router, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("X-Principal-Id", "admin-1")
	req.Header.Set("X-Tenant-Id", "11111111-1111-4111-8111-111111111111")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestWorkloadBindings_AdminRoutes(t *testing.T) {
	admin := []string{handler.PermissionWorkloadManage}
	newStub := func() *workloadStub {
		return &workloadStub{stubStore: &stubStore{rbacActions: admin}}
	}
	for name, body := range map[string]string{
		"wildcard audience": `{"workload_id":"w","allowed_audience":"*","allowed_actions":["A"],"reason_code":"ONBOARD"}`,
		"no actions":        `{"workload_id":"w","allowed_audience":"api","allowed_actions":[],"reason_code":"ONBOARD"}`,
		"wildcard action":   `{"workload_id":"w","allowed_audience":"api","allowed_actions":["*"],"reason_code":"ONBOARD"}`,
		"no workload id":    `{"allowed_audience":"api","allowed_actions":["A"],"reason_code":"ONBOARD"}`,
	} {
		s := newStub()
		if w := postWorkloadAdmin(workloadRouter(s), "/v1/admin/workload-bindings", body); w.Code != http.StatusBadRequest || s.saved != nil {
			t.Errorf("%s: want 400 and nothing saved, got %d", name, w.Code)
		}
	}

	s := newStub()
	w := postWorkloadAdmin(workloadRouter(s), "/v1/admin/workload-bindings",
		`{"workload_id":"ledger-sync","allowed_audience":"ledger-api","allowed_actions":["GL_JOURNAL_READ"],"reason_code":"ONBOARD","purpose":"nightly sync"}`)
	if w.Code != http.StatusOK || s.saved == nil || s.saved.TenantID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var ev map[string]any
	_ = json.Unmarshal(s.savedEvent.Value, &ev)
	if s.savedEvent.EventType != "iam.workload_binding.saved" || !bytes.Contains(s.savedEvent.Value, []byte("ONBOARD: nightly sync")) {
		t.Errorf("event %s %s: want the saved event carrying the reason", s.savedEvent.EventType, s.savedEvent.Value)
	}

	// Without the permission: 403.
	denied := &workloadStub{stubStore: &stubStore{denyAdmin: true}}
	if w := postWorkloadAdmin(workloadRouter(denied), "/v1/admin/workload-bindings",
		`{"workload_id":"w","allowed_audience":"api","allowed_actions":["A"],"reason_code":"X"}`); w.Code != http.StatusForbidden {
		t.Errorf("without %s: want 403, got %d", handler.PermissionWorkloadManage, w.Code)
	}

	// Deactivate: reason required (enforce is the default), 404 when absent.
	s = newStub()
	if w := postWorkloadAdmin(workloadRouter(s), "/v1/admin/workload-bindings/ledger-sync/deactivate", `{"reason_code":"COMPROMISED"}`); w.Code != http.StatusOK || s.setActive == nil || *s.setActive {
		t.Errorf("deactivate: %d %s", w.Code, w.Body.String())
	}
	missing := &workloadStub{stubStore: &stubStore{rbacActions: admin}, missing: true}
	if w := postWorkloadAdmin(workloadRouter(missing), "/v1/admin/workload-bindings/nope/deactivate", `{"reason_code":"X"}`); w.Code != http.StatusNotFound {
		t.Errorf("deactivate unknown: want 404, got %d", w.Code)
	}
}

// Production wraps the store in cache.Store, which must forward the binding
// capabilities, or the routes answer 503 there and nowhere in a stub test.
type cacheableWorkloadStub struct {
	cacheableStub
	ws *workloadStub
}

func (c cacheableWorkloadStub) SaveWorkloadBinding(ctx context.Context, wb domain.WorkloadBinding, ev domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	return c.ws.SaveWorkloadBinding(ctx, wb, ev)
}
func (c cacheableWorkloadStub) SetWorkloadBindingActive(ctx context.Context, id, tenant string, active bool, ev domain.OutboxMessage) (*domain.WorkloadBinding, error) {
	return c.ws.SetWorkloadBindingActive(ctx, id, tenant, active, ev)
}
func (c cacheableWorkloadStub) ListWorkloadBindings(ctx context.Context, tenant string) ([]domain.WorkloadBinding, error) {
	return c.ws.ListWorkloadBindings(ctx, tenant)
}
func (c cacheableWorkloadStub) IsBoundWorkload(ctx context.Context, principalID string) (bool, error) {
	return c.ws.IsBoundWorkload(ctx, principalID)
}

func TestWorkloadBindings_ThroughTheCache(t *testing.T) {
	ws := &workloadStub{stubStore: &stubStore{rbacActions: append([]string{handler.PermissionWorkloadManage}, stubAdminActions...)}}
	inner := cacheableWorkloadStub{cacheableStub{ws.stubStore}, ws}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(cache.New(inner, time.Second, zap.NewNop()), &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), platformID, false, zap.NewNop()))
	w := postWorkloadAdmin(r, "/v1/admin/workload-bindings",
		`{"workload_id":"ledger-sync","allowed_audience":"ledger-api","allowed_actions":["GL_JOURNAL_READ"],"reason_code":"ONBOARD"}`)
	if w.Code != http.StatusOK {
		t.Errorf("save through cache.Store: want 200, got %d: %s", w.Code, w.Body.String())
	}
}
