// Tests the /v1/authenticate HTTP surface, including its interaction with the
// canonical input contract middleware.
//
// The middleware is exercised here rather than mocked out because the
// exemption is the whole reason the endpoint is reachable: without it the
// contract refuses every request for lacking the identity headers this
// endpoint exists to make obtainable, and the refusal happens before any
// handler code runs — so a handler-only test would pass while the live service
// 401'd every login.
package context_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	identityctx "zoiko.io/identity-context-svc/internal/context"
	"zoiko.io/identity-context-svc/internal/credential"
	"zoiko.io/identity-context-svc/internal/domain"
	svcenvelope "zoiko.io/identity-context-svc/internal/envelope"
)

// newAuthRouter builds the router the service actually serves: contract
// middleware first, then routes.
func newAuthRouter(t *testing.T, store *mockCredentialStore, h *credential.Hasher) chi.Router {
	t.Helper()
	auth := identityctx.NewAuthenticator(zap.NewNop(), store, h, &mockTokenIssuer{}, newMockAuthEvents(), nil,
		identityctx.LockoutPolicy{MaxFailedAttempts: 3, LockDuration: time.Minute})

	r := chi.NewRouter()
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	identityctx.RegisterRoutes(r, identityctx.NewHandler(nil, auth, nil, nil, nil, zap.NewNop()))
	return r
}

func postJSON(t *testing.T, r chi.Router, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The exemption under test. A request carrying none of the contract's
// mandatory headers — which is every real login, since the caller has no
// identity yet — must reach the handler.
func TestAuthenticateRoute_ExemptFromCanonicalInputContract(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID,
		"email":     testEmail,
		"password":  testPassword,
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"the endpoint that produces identity cannot be gated on already having one")
	require.NotContains(t, rec.Body.String(), "envelope_incomplete")

	var resp domain.AuthenticateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.AccessToken)
	require.Equal(t, "Bearer", resp.TokenType)
}

// The exemption must be exactly one path wide. If it leaked to the rest of the
// service, /v1/context/resolve and the principal routes would stop enforcing
// the contract at the same time — and those callers DO already hold a verified
// identity, so there is nothing circular about requiring it of them.
func TestOtherRoutes_StillEnforceTheContract(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/context/resolve", map[string]string{
		"bearer_token": "anything",
	})

	require.NotEqual(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "envelope_incomplete",
		"exempting /v1/authenticate must not disarm the contract elsewhere")
}

func TestAuthenticateRoute_WrongPasswordIs401(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID,
		"email":     testEmail,
		"password":  "wrong",
	})

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

// Every rejection must be byte-identical on the wire. A body that differs by
// reason is an enumeration oracle even when the status code does not change.
func TestAuthenticateRoute_RejectionsAreIndistinguishableOnTheWire(t *testing.T) {
	h := testHasher(t)
	locked := time.Now().UTC().Add(time.Hour)
	goodHash, err := h.Hash(testPassword)
	require.NoError(t, err)
	principal := &domain.Principal{
		PrincipalID: "p-1", TenantID: testTenantID,
		IdentityProviderSubject: testSubject, Status: domain.PrincipalStatusActive,
	}

	stores := map[string]struct {
		store    *mockCredentialStore
		password string
	}{
		"unknown email": {&mockCredentialStore{lookupErr: domain.ErrPrincipalNotFound}, testPassword},
		"no credential": {&mockCredentialStore{principal: principal, lookupErr: domain.ErrCredentialNotFound}, testPassword},
		"locked": {&mockCredentialStore{principal: principal, credential: &domain.PrincipalCredential{
			CredentialID: "c-1", SecretHash: goodHash, LockedUntil: &locked,
		}}, testPassword},
		"wrong password": {&mockCredentialStore{principal: principal, credential: &domain.PrincipalCredential{
			CredentialID: "c-1", SecretHash: goodHash,
		}}, "nope"},
	}

	var seenStatus int
	var seenBody string
	for name, tc := range stores {
		t.Run(name, func(t *testing.T) {
			r := newAuthRouter(t, tc.store, h)
			rec := postJSON(t, r, "/v1/authenticate", map[string]string{
				"tenant_id": testTenantID,
				"email":     testEmail,
				"password":  tc.password,
			})

			require.Equal(t, http.StatusUnauthorized, rec.Code)
			if seenStatus == 0 {
				seenStatus, seenBody = rec.Code, rec.Body.String()
				return
			}
			require.Equal(t, seenStatus, rec.Code)
			require.Equal(t, seenBody, rec.Body.String(),
				"two rejection reasons produced different response bodies")
		})
	}
}

func TestAuthenticateRoute_MissingFieldIs400(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID,
		"email":     testEmail,
		// password omitted
	})

	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a malformed request is a client error, not a failed login")
}

func TestAuthenticateRoute_MalformedBodyIs400(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/authenticate", bytes.NewReader([]byte("{not json")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// A store outage must surface as 503, not 401. Collapsing them would tell a
// user to reset a password that was never the problem — the same 403-vs-503
// distinction the console keeps deliberately separate.
func TestAuthenticateRoute_StoreOutageIs503(t *testing.T) {
	h := testHasher(t)
	store := &mockCredentialStore{lookupErr: errContrived}
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID,
		"email":     testEmail,
		"password":  testPassword,
	})

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// A bearer token must never be cached by an intermediary.
func TestAuthenticateRoute_ResponseIsNotCacheable(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID,
		"email":     testEmail,
		"password":  testPassword,
	})

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

// The response must never carry the digest, the password, or anything else
// from the credential row.
func TestAuthenticateRoute_ResponseLeaksNoCredentialMaterial(t *testing.T) {
	h := testHasher(t)
	store := activeCredential(t, h, testPassword)
	r := newAuthRouter(t, store, h)

	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID,
		"email":     testEmail,
		"password":  testPassword,
	})

	body := rec.Body.String()
	require.NotContains(t, body, testPassword)
	require.NotContains(t, body, "argon2")
	require.NotContains(t, body, store.credential.SecretHash)
}
