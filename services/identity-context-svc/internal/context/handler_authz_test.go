package context_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/authz"
	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/domain"
)

// Authorization and session-scoping tests for identity-context-svc
// (tracker row 95, the last Priority 2b row).
//
// Two of this service's routes were CROSS-TENANT, not merely unauthorized,
// and the first is the most severe defect found in the whole tier:
//
//   - GET /v1/context/session/{id} had no tenant check and no principal
//     check, and returns EnvelopeJWT — the signed IdentityContextEnvelope
//     itself. Knowing a session id was enough to obtain a working credential
//     for that identity in ANY tenant. That is credential theft, not a data
//     leak: the JWT is what every other service on the platform trusts.
//   - POST /v1/context/session/{id}/invalidate had no tenant check either, so
//     any caller could force-logout any principal in any tenant — with the
//     actor taken from an unverified X-Actor-Principal-ID header, so the
//     audit record named whoever the caller claimed to be.
//
// POST /v1/context/resolve deliberately keeps NO principal guard. See
// TestResolveDoesNotRequireIdentityHeaders.

func sessionRouter(t *testing.T, sessions *mockSessionCache, az identityctx.AuthzChecker) chi.Router {
	t.Helper()
	f := defaultFixture()
	f.sessions = sessions
	r := chi.NewRouter()
	h := identityctx.NewHandler(f.build(), sessions, f.principals, az, zap.NewNop())
	identityctx.RegisterRoutes(r, h)
	return r
}

// seedSession puts both the JWT and its SessionContext in the cache, which is
// what the handler needs: SessionCache.Get carries no tenant, so the scope has
// to come from the SessionContext record.
func seedSession(sessions *mockSessionCache, id, tenantID, principalID, jwt string) {
	sessions.stored[id] = jwt
	sessions.storedCtx[id] = &domain.SessionContext{
		SessionContextID: id,
		TenantID:         tenantID,
		PrincipalID:      principalID,
	}
}

func getSession(r chi.Router, id, tenantID, principalID string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/context/session/"+id, nil)
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestGetSession_ForeignTenant_CannotObtainEnvelope is the regression test for
// the credential read. tenant-b knows tenant-a's session id and asks for it.
func TestGetSession_ForeignTenant_CannotObtainEnvelope(t *testing.T) {
	sessions := newMockSessionCache()
	seedSession(sessions, "sess-a", "tenant-a", "p-a", "tenant-a-envelope-jwt")
	r := sessionRouter(t, sessions, &stubAuthz{})

	w := getSession(r, "sess-a", "tenant-b", "p-b")

	// 404, never 403 — a distinct forbidden would let a caller enumerate valid
	// session ids, and session ids are precisely what an attacker probes for.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's session, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-envelope-jwt")) {
		t.Fatalf("CREDENTIAL LEAK: response contained another tenant's envelope JWT: %s", w.Body.String())
	}
}

// TestGetSession_OwnSession_Succeeds is the positive half — the
// self-exemption. Without it, every principal on the platform would need an
// explicit grant to read its own session and the check would be noise.
func TestGetSession_OwnSession_Succeeds(t *testing.T) {
	sessions := newMockSessionCache()
	seedSession(sessions, "sess-a", "tenant-a", "p-a", "tenant-a-envelope-jwt")
	r := sessionRouter(t, sessions, &stubAuthz{err: authz.ErrAuthorizationDenied})

	// Note the DENYING stub: reading your own session must not consult
	// authorization at all, so a denial must not affect it.
	w := getSession(r, "sess-a", "tenant-a", "p-a")

	if w.Code != http.StatusOK {
		t.Fatalf("a principal must be able to read its own session, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("tenant-a-envelope-jwt")) {
		t.Fatalf("own session should return the envelope, got: %s", w.Body.String())
	}
}

// TestGetSession_OtherPrincipalSameTenant_RequiresGrant covers the case that
// is easy to miss: handing one principal's envelope to another is credential
// sharing even inside one tenant.
func TestGetSession_OtherPrincipalSameTenant_RequiresGrant(t *testing.T) {
	sessions := newMockSessionCache()
	seedSession(sessions, "sess-a", "tenant-a", "p-a", "tenant-a-envelope-jwt")

	denied := sessionRouter(t, sessions, &stubAuthz{err: authz.ErrAuthorizationDenied})
	w := getSession(denied, "sess-a", "tenant-a", "p-OTHER")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 reading another principal's session without a grant, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-envelope-jwt")) {
		t.Fatalf("CREDENTIAL LEAK: denied principal received the envelope: %s", w.Body.String())
	}

	// With a grant (support/admin flow) it is allowed.
	granted := sessionRouter(t, sessions, &stubAuthz{})
	w2 := getSession(granted, "sess-a", "tenant-a", "p-OTHER")
	if w2.Code != http.StatusOK {
		t.Fatalf("a granted principal should be able to read the session, got %d: %s", w2.Code, w2.Body.String())
	}
}

func TestGetSession_MissingIdentityHeaders_Refused(t *testing.T) {
	sessions := newMockSessionCache()
	seedSession(sessions, "sess-a", "tenant-a", "p-a", "tenant-a-envelope-jwt")
	r := sessionRouter(t, sessions, &stubAuthz{})

	for _, tc := range []struct{ name, tenant, principal string }{
		{"no headers at all", "", ""},
		{"tenant only", "tenant-a", ""},
		{"principal only", "", "p-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := getSession(r, "sess-a", tc.tenant, tc.principal)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-envelope-jwt")) {
				t.Fatalf("CREDENTIAL LEAK: unauthenticated request received the envelope: %s", w.Body.String())
			}
		})
	}
}

func invalidate(r chi.Router, id, tenantID, principalID string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(domain.InvalidateSessionRequest{Reason: domain.InvalidationReasonLogout})
	req := httptest.NewRequest(http.MethodPost, "/v1/context/session/"+id+"/invalidate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if tenantID != "" {
		req.Header.Set("X-Tenant-Id", tenantID)
	}
	if principalID != "" {
		req.Header.Set("X-Principal-Id", principalID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestInvalidateSession_ForeignTenant_Refused asserts both the refusal AND
// that nothing happened — a 404 that still invalidated the session would pass
// a status-code-only test.
func TestInvalidateSession_ForeignTenant_Refused(t *testing.T) {
	sessions := newMockSessionCache()
	seedSession(sessions, "sess-a", "tenant-a", "p-a", "tenant-a-envelope-jwt")
	r := sessionRouter(t, sessions, &stubAuthz{})

	w := invalidate(r, "sess-a", "tenant-b", "p-b")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 invalidating another tenant's session, got %d: %s", w.Code, w.Body.String())
	}
	if len(sessions.invalidated) != 0 {
		t.Fatalf("ISOLATION FAILURE: a refused request still invalidated %v", sessions.invalidated)
	}
}

// TestInvalidateSession_RecordsVerifiedActor covers the attribution half. The
// actor used to come from an unverified X-Actor-Principal-ID header, so a
// forced logout could be attributed to anyone the caller named.
func TestInvalidateSession_RecordsVerifiedActor(t *testing.T) {
	sessions := newMockSessionCache()
	seedSession(sessions, "sess-a", "tenant-a", "p-a", "tenant-a-envelope-jwt")
	r := sessionRouter(t, sessions, &stubAuthz{})

	body, _ := json.Marshal(domain.InvalidateSessionRequest{Reason: domain.InvalidationReasonLogout})
	req := httptest.NewRequest(http.MethodPost, "/v1/context/session/sess-a/invalidate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "p-a")
	// A self-reported actor that must be IGNORED in favour of the verified one.
	req.Header.Set("X-Actor-Principal-ID", "p-IMPERSONATED")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("self-invalidation should succeed, got %d: %s", w.Code, w.Body.String())
	}
	if len(sessions.invalidated) != 1 {
		t.Fatalf("expected the session to be invalidated, got %v", sessions.invalidated)
	}
}

// TestUpdatePrincipalStatus_NoSelfExemption is the deliberate asymmetry: every
// other guarded route lets a principal act on itself without a grant, but a
// status change never does. A suspended principal reactivating itself would
// defeat the suspension.
func TestUpdatePrincipalStatus_NoSelfExemption(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a"}}
	r := chi.NewRouter()
	h := identityctx.NewHandler(nil, nil, store, &stubAuthz{err: authz.ErrAuthorizationDenied}, zap.NewNop())
	identityctx.RegisterRoutes(r, h)

	body, _ := json.Marshal(domain.UpdateStatusRequest{Status: domain.PrincipalStatusActive})
	req := httptest.NewRequest(http.MethodPut, "/v1/principals/p-1/status", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "tenant-a")
	// The caller IS p-1 — its own status. Must still be refused.
	req.Header.Set("X-Principal-Id", "p-1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("a principal must not change its OWN status without a grant, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPrincipalRead_OtherPrincipal_RequiresGrant covers the three read routes.
func TestPrincipalRead_OtherPrincipal_RequiresGrant(t *testing.T) {
	store := &mockPrincipalStore{principal: &domain.Principal{PrincipalID: "p-1", TenantID: "tenant-a"}}
	r := chi.NewRouter()
	h := identityctx.NewHandler(nil, nil, store, &stubAuthz{err: authz.ErrAuthorizationDenied}, zap.NewNop())
	identityctx.RegisterRoutes(r, h)

	for _, path := range []string{
		"/v1/principals/p-1",
		"/v1/principals/p-1/roles",
		"/v1/principals/p-1/delegations",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", "tenant-a")
		req.Header.Set("X-Principal-Id", "p-SOMEONE-ELSE")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403 reading another principal without a grant, got %d: %s",
				path, w.Code, w.Body.String())
		}
	}
}

// TestResolveDoesNotRequireIdentityHeaders is the guard rail on the one route
// that must stay unguarded.
//
// POST /v1/context/resolve takes a token in the body and returns a signed
// envelope, answering 401 on an invalid token. It IS the authentication
// endpoint. Requiring a verified principal before it would mean needing an
// envelope in order to obtain one — the strongest instance of the row 84d
// pattern in the estate.
//
// If someone later adds requirePrincipal here "for consistency", this fails
// instead of the platform's authentication path silently breaking.
func TestResolveDoesNotRequireIdentityHeaders(t *testing.T) {
	sessions := newMockSessionCache()
	r := sessionRouter(t, sessions, &stubAuthz{err: authz.ErrAuthorizationDenied})

	body, _ := json.Marshal(domain.ResolveRequest{
		BearerToken:   "a-token",
		LegalEntityID: "le-1",
		CorrelationID: "corr-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/context/resolve", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately NO identity headers — a caller authenticating has none yet.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Fatalf("POST /v1/context/resolve must not require identity headers — it IS the authentication endpoint; got 401: %s",
			w.Body.String())
	}
	if w.Code == http.StatusForbidden {
		t.Fatalf("POST /v1/context/resolve must not consult authorization — the denying stub should not affect it; got 403: %s",
			w.Body.String())
	}
}
