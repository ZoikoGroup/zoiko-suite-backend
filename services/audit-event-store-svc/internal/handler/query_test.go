package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	authzpkg "zoiko.io/audit-event-store-svc/internal/authz"
	"zoiko.io/audit-event-store-svc/internal/handler"
	"zoiko.io/audit-event-store-svc/internal/store"
)

type mockAuthz struct {
	denied bool
}

func (m *mockAuthz) CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	if m.denied {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func setupTestRouter(s store.Store, az handler.AuthzChecker, h *handler.QueryHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/v1/events", h.ListEvents)
	r.Get("/v1/events/{event_id}", h.GetEvent)
	return r
}

func TestListEvents_MissingTenantHeader(t *testing.T) {
	s := store.NewFakeStore()
	az := &mockAuthz{}
	h := handler.NewQueryHandler(s, az, zaptest.NewLogger(t))
	r := setupTestRouter(s, az, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestListEvents_MissingPrincipalHeader(t *testing.T) {
	s := store.NewFakeStore()
	az := &mockAuthz{}
	h := handler.NewQueryHandler(s, az, zaptest.NewLogger(t))
	r := setupTestRouter(s, az, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestListEvents_MismatchedTenantQuery(t *testing.T) {
	s := store.NewFakeStore()
	az := &mockAuthz{}
	h := handler.NewQueryHandler(s, az, zaptest.NewLogger(t))
	r := setupTestRouter(s, az, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/events?tenant_id=other-tenant", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "user-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestListEvents_AuthzDenied(t *testing.T) {
	s := store.NewFakeStore()
	az := &mockAuthz{denied: true}
	h := handler.NewQueryHandler(s, az, zaptest.NewLogger(t))
	r := setupTestRouter(s, az, h)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "user-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestListEvents_FilterAndPagination(t *testing.T) {
	s := store.NewFakeStore()
	ctx := context.Background()

	// Store test events
	e1 := &store.AuditEvent{
		EventID:       "evt-1",
		EventType:     "entity.status.changed",
		TenantID:      "tenant-1",
		LegalEntityID: "entity-1",
		PrincipalID:   "user-1",
		CorrelationID: "wf-1",
		Payload:       json.RawMessage(`{"status":"ACTIVE"}`),
		StoredAt:      time.Now().UTC(),
	}
	e2 := &store.AuditEvent{
		EventID:       "evt-2",
		EventType:     "invoice.approved",
		TenantID:      "tenant-1",
		LegalEntityID: "entity-1",
		PrincipalID:   "user-2",
		CorrelationID: "wf-2",
		Payload:       json.RawMessage(`{"amount":100}`),
		StoredAt:      time.Now().UTC(),
	}
	e3 := &store.AuditEvent{
		EventID:       "evt-3",
		EventType:     "invoice.approved",
		TenantID:      "tenant-2", // different tenant
		LegalEntityID: "entity-2",
		PrincipalID:   "user-3",
		CorrelationID: "wf-3",
		Payload:       json.RawMessage(`{"amount":200}`),
		StoredAt:      time.Now().UTC(),
	}

	require.NoError(t, s.Store(ctx, e1))
	require.NoError(t, s.Store(ctx, e2))
	require.NoError(t, s.Store(ctx, e3))

	az := &mockAuthz{}
	h := handler.NewQueryHandler(s, az, zaptest.NewLogger(t))
	r := setupTestRouter(s, az, h)

	// Query for tenant-1 events
	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "user-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Events []struct {
			EventID string `json:"event_id"`
		} `json:"events"`
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 2, resp.Total)
	assert.Len(t, resp.Events, 2)

	// Filter by action=invoice.approved
	req2 := httptest.NewRequest(http.MethodGet, "/v1/events?action=invoice.approved", nil)
	req2.Header.Set("X-Tenant-Id", "tenant-1")
	req2.Header.Set("X-Principal-Id", "user-1")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusOK, rec2.Code)
	var resp2 struct {
		Events []struct {
			EventID string `json:"event_id"`
		} `json:"events"`
		Total int `json:"total"`
	}
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&resp2))
	assert.Equal(t, 1, resp2.Total)
	assert.Equal(t, "evt-2", resp2.Events[0].EventID)
}

func TestGetEvent_SuccessAndNotFound(t *testing.T) {
	s := store.NewFakeStore()
	ctx := context.Background()

	e := &store.AuditEvent{
		EventID:       "evt-100",
		EventType:     "entity.created",
		TenantID:      "tenant-1",
		LegalEntityID: "entity-1",
		Payload:       json.RawMessage(`{"name":"test"}`),
	}
	require.NoError(t, s.Store(ctx, e))

	az := &mockAuthz{}
	h := handler.NewQueryHandler(s, az, zaptest.NewLogger(t))
	r := setupTestRouter(s, az, h)

	// Existing event
	req := httptest.NewRequest(http.MethodGet, "/v1/events/evt-100", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Principal-Id", "user-1")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		EventID string `json:"event_id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	assert.Equal(t, "evt-100", out.EventID)

	// Non-existing event
	req2 := httptest.NewRequest(http.MethodGet, "/v1/events/evt-999", nil)
	req2.Header.Set("X-Tenant-Id", "tenant-1")
	req2.Header.Set("X-Principal-Id", "user-1")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)

	assert.Equal(t, http.StatusNotFound, rec2.Code)
}
