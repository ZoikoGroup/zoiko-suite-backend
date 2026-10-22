package context_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	authpkg "zoiko.io/identity-context-svc/internal/auth"
	identityctx "zoiko.io/identity-context-svc/internal/context"
)

type fixedMFAStatus bool

func (f fixedMFAStatus) HasActiveTOTP(context.Context, string, string) (bool, error) {
	return bool(f), nil
}

// A verified client becomes the token's azp, and auth_time is the login.
func TestAuthenticate_VerifiedClientAndAuthTimeReachTheToken(t *testing.T) {
	h := newHarness(t, nil)
	h = newHarness(t, activeCredential(t, h.hasher, testPassword))
	h.auth.WithClients(authpkg.NewClientRegistry(map[string]string{"zoiko-console": "console-secret"}, false)).
		WithMFAStatus(fixedMFAStatus(true))

	req := request()
	req.ClientID, req.ClientSecret = "zoiko-console", "console-secret"
	before := time.Now().Add(-time.Second)
	resp, err := h.auth.Authenticate(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, h.issuer.issued, 1)
	g := h.issuer.issued[0]
	require.Equal(t, "zoiko-console", g.clientID)
	require.False(t, g.mfaDone, "a password is one factor")
	require.Equal(t, []string{"pwd"}, g.amr)
	require.True(t, g.authTime.After(before))
	require.True(t, resp.MFAEnrolled)
	require.Equal(t, g.authTime.Unix(), resp.AuthTime)
}

// A presented client that fails is refused before the account is touched:
// no lockout attempt is spent and no token is minted.
func TestAuthenticate_BadClientRefusedBeforeTheAccount(t *testing.T) {
	h := newHarness(t, nil)
	store := activeCredential(t, h.hasher, testPassword)
	h = newHarness(t, store)
	h.auth.WithClients(authpkg.NewClientRegistry(map[string]string{"zoiko-console": "console-secret"}, false))

	for name, c := range map[string][2]string{
		"wrong secret":   {"zoiko-console", "guess"},
		"unknown client": {"rogue-app", "console-secret"},
	} {
		req := request()
		req.ClientID, req.ClientSecret = c[0], c[1]
		_, err := h.auth.Authenticate(context.Background(), req)
		require.ErrorIs(t, err, identityctx.ErrInvalidClient, name)
	}
	require.Empty(t, h.issuer.issued)
	require.Empty(t, store.failureCalls, "a client failure must not count against the account")
	require.Empty(t, store.successCalls)
}

// required mode refuses a login that presents no client.
func TestAuthenticate_ClientRequired(t *testing.T) {
	h := newHarness(t, nil)
	h = newHarness(t, activeCredential(t, h.hasher, testPassword))
	h.auth.WithClients(authpkg.NewClientRegistry(map[string]string{"zoiko-console": "s"}, true))
	_, err := h.auth.Authenticate(context.Background(), request())
	require.ErrorIs(t, err, identityctx.ErrInvalidClient)
}

// With no registry wired, a presented client cannot be verified and is
// refused rather than silently ignored.
func TestAuthenticate_ClientWithoutRegistryRefused(t *testing.T) {
	h := newHarness(t, nil)
	h = newHarness(t, activeCredential(t, h.hasher, testPassword))
	req := request()
	req.ClientID, req.ClientSecret = "zoiko-console", "x"
	_, err := h.auth.Authenticate(context.Background(), req)
	require.ErrorIs(t, err, identityctx.ErrInvalidClient)

	// And a login with no client still works, with no azp.
	_, err = h.auth.Authenticate(context.Background(), request())
	require.NoError(t, err)
	require.Empty(t, h.issuer.issued[len(h.issuer.issued)-1].clientID)
}

// Through the real route: a client that does not authenticate is a 401
// invalid_client (found live on 14 Oct: it was falling through to 500).
func TestAuthenticateRoute_InvalidClientIs401(t *testing.T) {
	h := testHasher(t)
	r := newAuthRouter(t, activeCredential(t, h, testPassword), h)
	rec := postJSON(t, r, "/v1/authenticate", map[string]string{
		"tenant_id": testTenantID, "email": testEmail, "password": testPassword,
		"client_id": "zoiko-console", "client_secret": "wrong",
	})
	require.Equal(t, 401, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"invalid_client"`)
}
