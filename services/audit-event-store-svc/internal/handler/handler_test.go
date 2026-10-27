package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
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

// withTenant sets the X-Tenant-Id header requireTenant reads directly in tests
// (there is no envelope middleware mounted in this package's tests).
func withTenant(req *http.Request, tenantID string) *http.Request {
	req.Header.Set("X-Tenant-Id", tenantID)
	return req
}

// createArchiveFor creates an archive over [from,to] for tenantID through the
// real HTTP route and returns its archive_id.
func createArchiveFor(t *testing.T, r chi.Router, tenantID string, from, to int64) string {
	t.Helper()
	body := fmt.Sprintf(`{"from_sequence":%d,"to_sequence":%d}`, from, to)
	req := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(body))), tenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "setup: create archive for %s", tenantID)

	var a domain.Archive
	require.NoError(t, json.NewDecoder(w.Body).Decode(&a))
	require.NotEmpty(t, a.ArchiveID)
	return a.ArchiveID
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

// TestListEvents_TenantRequired: a missing tenant is 401, not 400. X-Tenant-Id
// is set by gateway-auth-svc, so its absence means the request never passed that
// verification (INV-01) — the audit doc's own "Segregation of Duties & Auth"
// states "X-Tenant-Id mandatory on all queries (401 if missing)".
func TestListEvents_TenantRequired(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	req := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
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

	req := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil)), "tenant-1")
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

	req := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil)), "tenant-1")
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

	req := withTenant(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil), "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestVerifyEventsChain_TenantRequired: the verify route is a query under the
// audit doc's tenant rule, so a resolved principal with no tenant fails closed
// with 401 rather than being served unscoped.
func TestVerifyEventsChain_TenantRequired(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	req := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreateArchive_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()

	noPrincipal := withTenant(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`)), "tenant-1")
	w := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	denied := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`))), "tenant-1")
	wDenied := httptest.NewRecorder()
	setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthorizationDenied}).ServeHTTP(wDenied, denied)
	assert.Equal(t, http.StatusForbidden, wDenied.Code)

	// No tenant: fail closed with 401, never an unscoped archive.
	noTenant := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`)))
	wNoTenant := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(wNoTenant, noTenant)
	assert.Equal(t, http.StatusUnauthorized, wNoTenant.Code)

	granted := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(`{"from_sequence":1,"to_sequence":1}`))), "tenant-1")
	wGranted := httptest.NewRecorder()
	setupTestRouter(t, s).ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusCreated, wGranted.Code)
}

func TestGetArchive_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	archiveID := createArchiveFor(t, r, "tenant-1", 1, 5)

	noPrincipal := withTenant(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archiveID, nil), "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	// No tenant: fail closed.
	noTenant := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archiveID, nil))
	wNoTenant := httptest.NewRecorder()
	r.ServeHTTP(wNoTenant, noTenant)
	assert.Equal(t, http.StatusUnauthorized, wNoTenant.Code)

	granted := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archiveID, nil)), "tenant-1")
	wGranted := httptest.NewRecorder()
	r.ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusOK, wGranted.Code)
}

func TestVerifyArchive_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	archiveID := createArchiveFor(t, r, "tenant-1", 1, 5)

	noPrincipal := withTenant(httptest.NewRequest(http.MethodPost, "/v1/archives/"+archiveID+"/verify", nil), "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	noTenant := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives/"+archiveID+"/verify", nil))
	wNoTenant := httptest.NewRecorder()
	r.ServeHTTP(wNoTenant, noTenant)
	assert.Equal(t, http.StatusUnauthorized, wNoTenant.Code)

	granted := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives/"+archiveID+"/verify", nil)), "tenant-1")
	wGranted := httptest.NewRecorder()
	r.ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusOK, wGranted.Code)
}

func TestListVerifications_RequiresAuth(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	archiveID := createArchiveFor(t, r, "tenant-1", 1, 5)

	noPrincipal := withTenant(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archiveID+"/verifications", nil), "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, noPrincipal)
	assert.Equal(t, http.StatusUnauthorized, w.Code)

	noTenant := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archiveID+"/verifications", nil))
	wNoTenant := httptest.NewRecorder()
	r.ServeHTTP(wNoTenant, noTenant)
	assert.Equal(t, http.StatusUnauthorized, wNoTenant.Code)

	granted := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archiveID+"/verifications", nil)), "tenant-1")
	wGranted := httptest.NewRecorder()
	r.ServeHTTP(wGranted, granted)
	assert.Equal(t, http.StatusOK, wGranted.Code)
}

// TestArchive_TenantIsolation is the HTTP-level proof of the tenant isolation
// added by migration 000005: an archive created for one tenant must not be
// readable, verifiable, or enumerable by another, and a missing tenant context
// must fail closed on every archive route.
func TestArchive_TenantIsolation(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	archA := createArchiveFor(t, r, "tenant-A", 1, 3)
	archB := createArchiveFor(t, r, "tenant-B", 1, 3)
	require.NotEqual(t, archA, archB, "each tenant's archive must be a distinct record")

	// tenant A can access its own archive, and it records the right tenant.
	own := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archA, nil)), "tenant-A")
	wOwn := httptest.NewRecorder()
	r.ServeHTTP(wOwn, own)
	require.Equal(t, http.StatusOK, wOwn.Code)
	var a domain.Archive
	require.NoError(t, json.NewDecoder(wOwn.Body).Decode(&a))
	assert.Equal(t, "tenant-A", a.TenantID, "archive creation must record the creating tenant")

	// tenant A cannot read tenant B's archive — 404, indistinguishable from a
	// genuinely missing archive.
	crossGet := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archB, nil)), "tenant-A")
	wCrossGet := httptest.NewRecorder()
	r.ServeHTTP(wCrossGet, crossGet)
	assert.Equal(t, http.StatusNotFound, wCrossGet.Code, "tenant A must not read tenant B's archive")

	// tenant A cannot verify tenant B's archive.
	crossVerify := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives/"+archB+"/verify", nil)), "tenant-A")
	wCrossVerify := httptest.NewRecorder()
	r.ServeHTTP(wCrossVerify, crossVerify)
	assert.Equal(t, http.StatusNotFound, wCrossVerify.Code, "tenant A must not verify tenant B's archive")

	// tenant A cannot enumerate tenant B's verification history.
	crossList := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archB+"/verifications", nil)), "tenant-A")
	wCrossList := httptest.NewRecorder()
	r.ServeHTTP(wCrossList, crossList)
	assert.Equal(t, http.StatusNotFound, wCrossList.Code, "tenant A must not read tenant B's verification history")

	// Own verification and history remain reachable for the owning tenant.
	ownVerify := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/archives/"+archA+"/verify", nil)), "tenant-A")
	wOwnVerify := httptest.NewRecorder()
	r.ServeHTTP(wOwnVerify, ownVerify)
	require.Equal(t, http.StatusOK, wOwnVerify.Code)

	var verification domain.ArchiveVerification
	require.NoError(t, json.NewDecoder(wOwnVerify.Body).Decode(&verification))
	assert.Equal(t, "tenant-A", verification.TenantID, "verification must be recorded under the verifying tenant")

	ownList := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/"+archA+"/verifications", nil)), "tenant-A")
	wOwnList := httptest.NewRecorder()
	r.ServeHTTP(wOwnList, ownList)
	require.Equal(t, http.StatusOK, wOwnList.Code)
	var history []domain.ArchiveVerification
	require.NoError(t, json.NewDecoder(wOwnList.Body).Decode(&history))
	require.Len(t, history, 1)
	assert.Equal(t, "tenant-A", history[0].TenantID)

	// Missing tenant fails closed on every archive route.
	for _, tc := range []struct {
		name, method, path string
	}{
		{"create", http.MethodPost, "/v1/archives"},
		{"get", http.MethodGet, "/v1/archives/" + archA},
		{"verify", http.MethodPost, "/v1/archives/" + archA + "/verify"},
		{"verifications", http.MethodGet, "/v1/archives/" + archA + "/verifications"},
	} {
		var body *strings.Reader
		if tc.method == http.MethodPost {
			body = strings.NewReader(`{"from_sequence":1,"to_sequence":3}`)
		}
		var req *http.Request
		if body != nil {
			req = withPrincipal(httptest.NewRequest(tc.method, tc.path, body))
		} else {
			req = withPrincipal(httptest.NewRequest(tc.method, tc.path, nil))
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equalf(t, http.StatusUnauthorized, w.Code, "missing tenant must fail closed on %s", tc.name)
	}
}

