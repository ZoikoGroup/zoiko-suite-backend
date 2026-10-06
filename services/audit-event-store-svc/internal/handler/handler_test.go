package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"zoiko.io/audit-event-store-svc/internal/authz"
	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/handler"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// stubAuthz defaults to "always granted" so every pre-existing test keeps
// exercising only the store/query logic it was written for, exactly as
// before this integration existed. Tests that care about the authz gate
// itself construct their own instance directly.
type stubAuthz struct{ err error }

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

func setupTestRouter(t *testing.T, s *store.FakeStore) chi.Router {
	t.Helper()
	return setupTestRouterWithAuthz(t, s, &stubAuthz{})
}

func setupTestRouterWithAuthz(t *testing.T, s *store.FakeStore, az handler.AuthzChecker) chi.Router {
	t.Helper()
	log := zaptest.NewLogger(t)
	h := handler.New(s, az, log)
	r := chi.NewRouter()
	handler.RegisterRoutes(r, h)
	return r
}

// withPrincipal sets the header requirePrincipal reads directly in tests,
// which don't mount svcenvelope.Middleware (see handler.requirePrincipal's
// doc comment on the fallback).
func withPrincipal(req *http.Request) *http.Request {
	req.Header.Set("X-Principal-Id", "test-principal")
	return req
}

func seedTestEvent(t *testing.T, s *store.FakeStore, eventID, eventType, tenantID, entityID, principalID, corrID, payloadJSON string) {
	t.Helper()
	err := s.Store(context.Background(), &store.AuditEvent{
		EventID:       eventID,
		EventType:     eventType,
		TenantID:      tenantID,
		LegalEntityID: entityID,
		PrincipalID:   principalID,
		SourceService: "test-svc",
		SchemaVersion: "1.0",
		CorrelationID: corrID,
		Payload:       json.RawMessage(payloadJSON),
	})
	require.NoError(t, err)
}

func TestListEvents_TenantRequired(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var errResp map[string]string
	require.NoError(t, json.NewDecoder(w.Body).Decode(&errResp))
	assert.Equal(t, "tenant_required", errResp["code"])
}

func TestListEvents_RequiresPrincipal(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestListEvents_DeniedWhenNotAuthorized(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthorizationDenied})

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListEvents_FailsClosedWhenAuthzUnavailable(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestListEvents_SuccessAndFiltering(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	seedTestEvent(t, s, "evt-1", "identity.context.resolved", "tenant-1", "entity-1", "user-1", "corr-1", `{"domain":"tax","resource":"TaxReturn","status":"COMMITTED"}`)
	seedTestEvent(t, s, "evt-2", "entity.status.changed", "tenant-1", "entity-1", "user-2", "corr-2", `{"domain":"legal","resource":"Contract","status":"AUTHORIZED"}`)
	seedTestEvent(t, s, "evt-3", "audit.engagement.created", "tenant-2", "entity-2", "user-3", "corr-3", `{"domain":"compliance","resource":"Engagement","status":"AUTHORIZED"}`)

	// Query for tenant-1
	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var res domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(w.Body).Decode(&res))

	assert.Equal(t, int64(2), res.Total)
	assert.Len(t, res.Events, 2)
	assert.True(t, res.HashChainValid)
	assert.Equal(t, "evt-2", res.Events[0].ID) // newest first
	assert.Equal(t, "legal", res.Events[0].Domain)
	assert.Equal(t, "Contract", res.Events[0].Resource)
	assert.Equal(t, "AUTHORIZED", res.Events[0].Status)
	assert.Equal(t, "evt-1", res.Events[1].ID)

	// Filter by actor user-1
	reqActor := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events?actor=user-1", nil))
	reqActor.Header.Set("X-Tenant-Id", "tenant-1")
	wActor := httptest.NewRecorder()
	r.ServeHTTP(wActor, reqActor)

	assert.Equal(t, http.StatusOK, wActor.Code)
	var resActor domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(wActor.Body).Decode(&resActor))
	assert.Equal(t, int64(1), resActor.Total)
	assert.Equal(t, "evt-1", resActor.Events[0].ID)

	// Filter by action entity.status.changed
	reqAction := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events?action=entity.status.changed", nil))
	reqAction.Header.Set("X-Tenant-Id", "tenant-1")
	wAction := httptest.NewRecorder()
	r.ServeHTTP(wAction, reqAction)

	assert.Equal(t, http.StatusOK, wAction.Code)
	var resAction domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(wAction.Body).Decode(&resAction))
	assert.Equal(t, int64(1), resAction.Total)
	assert.Equal(t, "evt-2", resAction.Events[0].ID)
}

func TestListEvents_TenantIsolation(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	seedTestEvent(t, s, "evt-a", "test.event", "tenant-A", "entity-A", "user-A", "corr-A", `{"val":"A"}`)
	seedTestEvent(t, s, "evt-b", "test.event", "tenant-B", "entity-B", "user-B", "corr-B", `{"val":"B"}`)

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	req.Header.Set("X-Tenant-Id", "tenant-B")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var res domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(w.Body).Decode(&res))
	assert.Equal(t, int64(1), res.Total)
	assert.Equal(t, "evt-b", res.Events[0].ID)
	assert.Equal(t, "tenant-B", res.Events[0].TenantID)
}

func TestListEvents_Pagination(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	for i := 1; i <= 5; i++ {
		seedTestEvent(t, s, "evt-"+string(rune('0'+i)), "test.event", "tenant-1", "entity-1", "user-1", "corr-1", `{}`)
	}

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events?limit=2&offset=0", nil))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var res1 domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(w.Body).Decode(&res1))
	assert.Equal(t, int64(5), res1.Total)
	assert.Len(t, res1.Events, 2)
	assert.Equal(t, "evt-5", res1.Events[0].ID)
	assert.Equal(t, "evt-4", res1.Events[1].ID)

	req2 := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events?limit=2&offset=2", nil))
	req2.Header.Set("X-Tenant-Id", "tenant-1")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)

	var res2 domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(w2.Body).Decode(&res2))
	assert.Equal(t, int64(5), res2.Total)
	assert.Len(t, res2.Events, 2)
	assert.Equal(t, "evt-3", res2.Events[0].ID)
	assert.Equal(t, "evt-2", res2.Events[1].ID)
}

func TestVerifyEventsChain_Empty(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, true, resp["verified"])
	assert.Equal(t, float64(0), resp["checkedEvents"])
	assert.NotEmpty(t, resp["timestamp"])
}

func TestVerifyEventsChain_Intact(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	seedTestEvent(t, s, "evt-1", "identity.context.resolved", "tenant-1", "entity-1", "user-1", "corr-1", `{}`)
	seedTestEvent(t, s, "evt-2", "entity.status.changed", "tenant-1", "entity-1", "user-2", "corr-2", `{}`)

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
	assert.Equal(t, true, resp["verified"])
	assert.Equal(t, float64(2), resp["checkedEvents"])
	assert.Equal(t, float64(2), resp["checked_events"])
	assert.NotEmpty(t, resp["timestamp"])
}

func TestVerifyEventsChain_RequiresPrincipal(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	req := httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreateArchive_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()

	noPrincipal := httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`))
	w := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	denied := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`)))
	wDenied := httptest.NewRecorder()
	setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthorizationDenied}).ServeHTTP(wDenied, denied)
	assert.Equal(t, http.StatusForbidden, wDenied.Code)

	granted := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`)))
	wGranted := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusCreated, wGranted.Code)
}

func TestGetArchive_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()

	noPrincipal := httptest.NewRequest(http.MethodGet, "/v1/archives/arch-1", nil)
	w := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	granted := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/arch-1", nil))
	wGranted := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusOK, wGranted.Code)
}

func TestVerifyArchive_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()

	noPrincipal := httptest.NewRequest(http.MethodPost, "/v1/archives/arch-1/verify", nil)
	w := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	granted := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives/arch-1/verify", nil))
	wGranted := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusOK, wGranted.Code)
}

func TestListVerifications_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()

	noPrincipal := httptest.NewRequest(http.MethodGet, "/v1/archives/arch-1/verifications", nil)
	w := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	granted := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/arch-1/verifications", nil))
	wGranted := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusOK, wGranted.Code)
}

