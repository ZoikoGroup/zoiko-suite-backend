package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/audit-event-store-svc/internal/authz"
	"zoiko.io/audit-event-store-svc/internal/domain"
	"zoiko.io/audit-event-store-svc/internal/store"
)

// POST /v1/archives and Idempotency-Key. These tests run the real handler over
// store.FakeStore, which mirrors the PgStore contract (tenant-scoped key,
// principal-and-range fingerprint, original archive replayed). The transactional
// behaviour — concurrency, rollback, row-level security — is proved against a
// real database in cmd/server/archive_idempotency_integration_test.go.

func postArchive(t *testing.T, r http.Handler, tenant, principal, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(body))
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func archiveOf(t *testing.T, w *httptest.ResponseRecorder) domain.Archive {
	t.Helper()
	var a domain.Archive
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a), w.Body.String())
	return a
}

const rng = `{"from_sequence":1,"to_sequence":5}`

func TestCreateArchive_ExactReplay_ReturnsTheOriginalArchive(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)

	first := postArchive(t, r, "tenant-1", "p1", "key-1", rng)
	second := postArchive(t, r, "tenant-1", "p1", "key-1", rng)

	require.Equal(t, http.StatusCreated, first.Code)
	require.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, archiveOf(t, first).ArchiveID, archiveOf(t, second).ArchiveID, "a retry gets the original archive")
	assert.Empty(t, first.Header().Get("X-Idempotent-Replay"))
	assert.Equal(t, "true", second.Header().Get("X-Idempotent-Replay"))

	// Exactly one record exists: the id a second archive would have received
	// is unknown.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/archives/arch-2", nil)), "tenant-1"))
	assert.Equal(t, http.StatusNotFound, w.Code, "a replay must not create a second archive")
}

func TestCreateArchive_SameKeyDifferentRequest_Is409AndReturnsNothingElse(t *testing.T) {
	r := setupTestRouter(t, store.NewFakeStore())
	first := postArchive(t, r, "tenant-1", "p1", "key-1", rng)
	require.Equal(t, http.StatusCreated, first.Code)

	other := postArchive(t, r, "tenant-1", "p1", "key-1", `{"from_sequence":1,"to_sequence":9}`)
	assert.Equal(t, http.StatusConflict, other.Code)
	assert.Contains(t, other.Body.String(), "idempotency_mismatch")
	assert.NotContains(t, other.Body.String(), archiveOf(t, first).ArchiveID, "the other request's archive must not be handed over")
}

func TestCreateArchive_SameKeyDifferentPrincipal_IsAMismatchNotAReplay(t *testing.T) {
	r := setupTestRouter(t, store.NewFakeStore())
	first := postArchive(t, r, "tenant-1", "principal-a", "key-1", rng)
	require.Equal(t, http.StatusCreated, first.Code)

	second := postArchive(t, r, "tenant-1", "principal-b", "key-1", rng)
	assert.Equal(t, http.StatusConflict, second.Code)
	assert.Contains(t, second.Body.String(), "idempotency_mismatch")
}

func TestCreateArchive_SameKeyDifferentTenant_IsIndependent(t *testing.T) {
	r := setupTestRouter(t, store.NewFakeStore())
	a := postArchive(t, r, "tenant-a", "p1", "shared-key", rng)
	b := postArchive(t, r, "tenant-b", "p1", "shared-key", rng)

	require.Equal(t, http.StatusCreated, a.Code)
	require.Equal(t, http.StatusCreated, b.Code)
	assert.NotEqual(t, archiveOf(t, a).ArchiveID, archiveOf(t, b).ArchiveID)
	assert.Equal(t, "tenant-b", archiveOf(t, b).TenantID)
	assert.Empty(t, b.Header().Get("X-Idempotent-Replay"), "another tenant's key is never a replay")
}

// A stored replay must not be a way round authorization: the principal,
// the grant and the tenant are all established BEFORE the store is asked.
func TestCreateArchive_Replay_DoesNotBypassAuthorization(t *testing.T) {
	s := store.NewFakeStore()
	granted := setupTestRouter(t, s)
	first := postArchive(t, granted, "tenant-1", "p1", "key-1", rng)
	require.Equal(t, http.StatusCreated, first.Code)
	originalID := archiveOf(t, first).ArchiveID

	denied := setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthorizationDenied})
	w := postArchive(t, denied, "tenant-1", "p1", "key-1", rng)
	assert.Equal(t, http.StatusForbidden, w.Code, "a denied principal is refused even with a valid stored key")
	assert.NotContains(t, w.Body.String(), originalID)

	down := setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthzServiceUnavailable})
	w = postArchive(t, down, "tenant-1", "p1", "key-1", rng)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "authorization outage still fails closed on a replay")
	assert.NotContains(t, w.Body.String(), originalID)

	w = postArchive(t, granted, "tenant-1", "", "key-1", rng)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "no principal: 401 even with a valid stored key")
	assert.NotContains(t, w.Body.String(), originalID)

	w = postArchive(t, granted, "", "p1", "key-1", rng)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "no tenant: 401 even with a valid stored key")
	assert.NotContains(t, w.Body.String(), originalID)
}

func TestCreateArchive_WorkloadIdentityStillCannotStandInForThePrincipal(t *testing.T) {
	r := setupTestRouter(t, store.NewFakeStore())
	req := httptest.NewRequest(http.MethodPost, "/v1/archives", strings.NewReader(rng))
	req.Header.Set("X-Tenant-Id", "tenant-1")
	req.Header.Set("X-Workload-Id", "some-workload")
	req.Header.Set("Idempotency-Key", "key-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreateArchive_OverlongKey_Is400(t *testing.T) {
	r := setupTestRouter(t, store.NewFakeStore())
	w := postArchive(t, r, "tenant-1", "p1", strings.Repeat("k", 256), rng)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_idempotency_key")

	w = postArchive(t, r, "tenant-1", "p1", strings.Repeat("k", 255), rng)
	assert.Equal(t, http.StatusCreated, w.Code, "255 characters is the limit and is accepted")
}

func TestCreateArchive_WithoutAKey_BehavesAsBefore(t *testing.T) {
	r := setupTestRouter(t, store.NewFakeStore())
	a := postArchive(t, r, "tenant-1", "p1", "", rng)
	b := postArchive(t, r, "tenant-1", "p1", "", rng)
	require.Equal(t, http.StatusCreated, a.Code)
	require.Equal(t, http.StatusCreated, b.Code)
	assert.NotEqual(t, archiveOf(t, a).ArchiveID, archiveOf(t, b).ArchiveID,
		"no key, no deduplication: unchanged behaviour")
}
