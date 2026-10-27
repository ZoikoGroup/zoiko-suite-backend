package handler_test

// Tests for: query-parameter validation (400 instead of silently unfiltered),
// computed hash_chain_valid, payload/link/sequence tamper detection on
// POST /v1/events/verify, and the RBAC action each route requests.

import (
	"context"
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

func getEvents(t *testing.T, r http.Handler, tenant, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := withTenant(withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/events"+query, nil)), tenant)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeResult(t *testing.T, w *httptest.ResponseRecorder) domain.QueryEventsResult {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var res domain.QueryEventsResult
	require.NoError(t, json.NewDecoder(w.Body).Decode(&res))
	return res
}

func TestListEvents_MalformedParametersAreRejected_NotIgnored(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	seedTestEvent(t, s, "evt-1", "entity.status.changed", "tenant-1", "entity-1", "user-1", "corr-1", `{}`)

	for _, tc := range []struct{ name, query, code string }{
		{"malformed from", "?from=yesterday", "invalid_from"},
		{"date-only from", "?from=2026-01-01", "invalid_from"},
		{"malformed to", "?to=2026-13-45T00:00:00Z", "invalid_to"},
		{"non-numeric limit", "?limit=abc", "invalid_limit"},
		{"zero limit", "?limit=0", "invalid_limit"},
		{"negative limit", "?limit=-5", "invalid_limit"},
		{"non-numeric offset", "?offset=x", "invalid_offset"},
		{"negative offset", "?offset=-1", "invalid_offset"},
		{"from after to", "?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z", "invalid_range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := getEvents(t, r, "tenant-1", tc.query)
			require.Equal(t, http.StatusBadRequest, w.Code, "must not silently fall back to an unfiltered query")
			var e map[string]string
			require.NoError(t, json.NewDecoder(w.Body).Decode(&e))
			assert.Equal(t, tc.code, e["code"], "service's canonical {code,message} error body")
			assert.NotEmpty(t, e["message"])
		})
	}
}

func TestListEvents_ValidParametersStillWork(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	seedTestEvent(t, s, "evt-1", "entity.status.changed", "tenant-1", "entity-1", "user-1", "corr-1", `{}`)
	seedTestEvent(t, s, "evt-2", "identity.context.resolved", "tenant-1", "entity-2", "user-2", "corr-2", `{}`)

	res := decodeResult(t, getEvents(t, r, "tenant-1", "?from=2000-01-01T00:00:00Z&to=2099-01-01T00:00:00Z&limit=1&offset=0"))
	assert.Equal(t, int64(2), res.Total)
	assert.Len(t, res.Events, 1, "limit honoured, total still reported")

	res = decodeResult(t, getEvents(t, r, "tenant-1", "?entity=entity-2"))
	assert.Len(t, res.Events, 1)
	assert.Equal(t, "evt-2", res.Events[0].ID)

	res = decodeResult(t, getEvents(t, r, "tenant-1", "?workflow=corr-1"))
	assert.Len(t, res.Events, 1)
	assert.Equal(t, "evt-1", res.Events[0].ID)

	// A limit above the documented page ceiling is clamped, not rejected.
	assert.Equal(t, http.StatusOK, getEvents(t, r, "tenant-1", "?limit=100000").Code)
	// An empty value is the same as an absent one.
	assert.Equal(t, http.StatusOK, getEvents(t, r, "tenant-1", "?limit=&from=").Code)
}

// Validation must not run ahead of authentication/authorization.
func TestListEvents_ParameterValidationDoesNotPrecedeAuth(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	req := httptest.NewRequest(http.MethodGet, "/v1/events?limit=abc", nil)
	req.Header.Set("X-Tenant-Id", "tenant-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code, "no principal: 401 whatever the parameters say")

	rd := setupTestRouterWithAuthz(t, s, &stubAuthz{err: authz.ErrAuthorizationDenied})
	w = getEvents(t, rd, "tenant-1", "?limit=abc")
	assert.Equal(t, http.StatusForbidden, w.Code, "denied: 403 whatever the parameters say")
}

func seedChain(t *testing.T, s *store.FakeStore) {
	t.Helper()
	seedTestEvent(t, s, "evt-1", "entity.status.changed", "tenant-1", "entity-1", "user-1", "corr-1", `{"a":1}`)
	seedTestEvent(t, s, "evt-2", "entity.status.changed", "tenant-1", "entity-1", "user-1", "corr-2", `{"a":2}`)
	seedTestEvent(t, s, "evt-3", "entity.status.changed", "tenant-1", "entity-1", "user-1", "corr-3", `{"a":3}`)
}

func TestListEvents_HashChainValidIsComputed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*store.FakeStore)
	}{
		{"modified payload", func(s *store.FakeStore) { s.TamperPayloadForTest("evt-2", json.RawMessage(`{"a":999}`)) }},
		{"modified stored hash", func(s *store.FakeStore) { s.TamperHashForTest("evt-2", "tampered") }},
		{"broken previous-hash link", func(s *store.FakeStore) { s.TamperPreviousHashForTest("evt-2", "not-the-previous-hash") }},
		{"missing predecessor (sequence gap)", func(s *store.FakeStore) { s.DeleteForTest("evt-2") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewFakeStore()
			r := setupTestRouter(t, s)
			seedChain(t, s)
			assert.True(t, decodeResult(t, getEvents(t, r, "tenant-1", "")).HashChainValid, "an intact chain is valid")

			tc.tamper(s)
			assert.False(t, decodeResult(t, getEvents(t, r, "tenant-1", "")).HashChainValid,
				"hash_chain_valid must be false once the returned events no longer verify")
		})
	}
}

// hash_chain_valid is a verdict about the returned events, not the whole chain.
func TestListEvents_HashChainValidScopeIsTheReturnedEvents(t *testing.T) {
	s := store.NewFakeStore()
	r := setupTestRouter(t, s)
	seedChain(t, s)
	s.TamperPayloadForTest("evt-3", json.RawMessage(`{"a":999}`))

	// evt-1 alone is intact, evt-3 is not.
	assert.True(t, decodeResult(t, getEvents(t, r, "tenant-1", "?workflow=corr-1")).HashChainValid)
	assert.False(t, decodeResult(t, getEvents(t, r, "tenant-1", "?workflow=corr-3")).HashChainValid)
	// An empty result is vacuously valid.
	assert.True(t, decodeResult(t, getEvents(t, r, "tenant-1", "?workflow=nothing")).HashChainValid)
}

func TestVerifyEventsChain_DetectsTampering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(*store.FakeStore)
	}{
		{"modified payload (hash untouched)", func(s *store.FakeStore) { s.TamperPayloadForTest("evt-2", json.RawMessage(`{"a":999}`)) }},
		{"modified stored hash", func(s *store.FakeStore) { s.TamperHashForTest("evt-2", "tampered") }},
		{"broken previous-hash link", func(s *store.FakeStore) { s.TamperPreviousHashForTest("evt-3", "broken") }},
		{"broken sequence (deleted row)", func(s *store.FakeStore) { s.DeleteForTest("evt-2") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := store.NewFakeStore()
			r := setupTestRouter(t, s)
			seedChain(t, s)

			verify := func() map[string]interface{} {
				req := withTenant(withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/events/verify", nil)), "tenant-1")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code)
				var resp map[string]interface{}
				require.NoError(t, json.NewDecoder(w.Body).Decode(&resp))
				return resp
			}
			assert.Equal(t, true, verify()["verified"])
			tc.tamper(s)
			assert.Equal(t, false, verify()["verified"])
		})
	}
}

// recordingAuthz records every (scope, action) pair the handler asks about.
type recordingAuthz struct {
	err     error
	actions []string
	scopes  []string
}

func (a *recordingAuthz) CheckAllowed(_ context.Context, _, scope, action string) error {
	a.scopes = append(a.scopes, scope)
	a.actions = append(a.actions, action)
	return a.err
}

func TestRBAC_EachRouteRequestsItsDocumentedAction(t *testing.T) {
	routes := []struct {
		method, path, body, action string
	}{
		{http.MethodGet, "/v1/events", "", "AUDIT_EVENT_READ"},
		{http.MethodPost, "/v1/events/verify", "", "AUDIT_EVENT_READ"},
		{http.MethodPost, "/v1/archives", `{"from_sequence":1,"to_sequence":1}`, "AUDIT_ARCHIVE_MANAGE"},
		{http.MethodGet, "/v1/archives/arch-1", "", "AUDIT_ARCHIVE_MANAGE"},
		{http.MethodPost, "/v1/archives/arch-1/verify", "", "AUDIT_ARCHIVE_MANAGE"},
		{http.MethodGet, "/v1/archives/arch-1/verifications", "", "AUDIT_ARCHIVE_MANAGE"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// Denied: 403, and the documented action was the one asked about.
			az := &recordingAuthz{err: authz.ErrAuthorizationDenied}
			r := setupTestRouterWithAuthz(t, store.NewFakeStore(), az)
			req := withTenant(withPrincipal(httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))), "tenant-1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			assert.Equal(t, http.StatusForbidden, w.Code)
			require.Equal(t, []string{rt.action}, az.actions)
			assert.Equal(t, []string{"00000000-0000-0000-0000-00000000f001"}, az.scopes, "checked in the platform scope")

			// Dependency failure: 503, fail closed.
			az = &recordingAuthz{err: assert.AnError}
			r = setupTestRouterWithAuthz(t, store.NewFakeStore(), az)
			w = httptest.NewRecorder()
			r.ServeHTTP(w, withTenant(withPrincipal(httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))), "tenant-1"))
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		})
	}

	// Allowed: reads succeed with the grant.
	s := store.NewFakeStore()
	r := setupTestRouterWithAuthz(t, s, &recordingAuthz{})
	assert.Equal(t, http.StatusOK, getEvents(t, r, "tenant-1", "").Code)
}
