package oidc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/oidc"
	"zoiko.io/identity-context-svc/internal/webauthn"
	"zoiko.io/identity-context-svc/internal/webauthn/webauthntest"
)

// The hosted login's passkey second step (AU-11, ZS-IAM-001 §17), with the
// real WebAuthn service and a software authenticator.

const (
	pkRPID   = "idp.test"
	pkOrigin = "http://idp.test"
)

type passkeyLogin struct {
	*harness
	keys *webauthntest.MemoryStore
	auth *webauthntest.Authenticator
}

func newPasskeyLogin(t *testing.T, register bool) *passkeyLogin {
	t.Helper()
	keys := webauthntest.NewMemoryStore()
	svc, err := webauthn.New(webauthn.Config{RPID: pkRPID, RPOrigins: []string{pkOrigin}}, keys,
		webauthn.NewMemoryCeremonies(), nil, zap.NewNop())
	require.NoError(t, err)
	h := newHarness(t, func(o *oidc.Options) { o.Passkeys = svc })
	pl := &passkeyLogin{harness: h, keys: keys, auth: webauthntest.New(t, pkRPID, pkOrigin)}
	if register {
		p := h.store.principals[principalID]
		opts, handle, err := svc.BeginRegistration(context.Background(), p)
		require.NoError(t, err)
		_, err = svc.FinishRegistration(context.Background(), p, handle, "laptop", pl.auth.Create(t, opts), "c")
		require.NoError(t, err)
	}
	return pl
}

// password runs authorize + the password step and returns the second-step
// page with its pending id, CSRF token and cookie.
func (pl *passkeyLogin) password(t *testing.T) (page *httptest.ResponseRecorder, pending, csrf string, c *http.Cookie) {
	t.Helper()
	w := pl.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+authorizeQuery("spa", "openid").Encode(), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	pend, tok := pendingRe.FindStringSubmatch(w.Body.String()), csrfRe.FindStringSubmatch(w.Body.String())
	c = cookie(w, "zoiko_login_csrf")
	page = pl.do(form(http.MethodPost, "/oauth2/login", url.Values{"pending": {pend[1]}, "csrf": {tok[1]}, "tenant": {tenant},
		"email": {"alice@corp.test"}, "password": {"correct horse battery"}}, c))
	return page, pend[1], tok[1], c
}

func (pl *passkeyLogin) begin(t *testing.T, pending, csrf string, c *http.Cookie) (json.RawMessage, string) {
	t.Helper()
	w := pl.do(form(http.MethodPost, "/oauth2/login/passkey/begin", url.Values{"pending": {pending}, "csrf": {csrf}}, c))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var b struct {
		Options json.RawMessage `json:"options"`
		Session string          `json:"session"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	return b.Options, b.Session
}

func (pl *passkeyLogin) finish(pending, csrf, session string, assertion json.RawMessage, c *http.Cookie) *httptest.ResponseRecorder {
	req := form(http.MethodPost, "/oauth2/login/passkey/finish",
		url.Values{"pending": {pending}, "csrf": {csrf}, "assertion": {string(assertion)}}, c)
	req.Header.Set("X-Passkey-Session", session)
	return pl.do(req)
}

func TestAU11_HostedLoginCompletesWithAPasskey(t *testing.T) {
	pl := newPasskeyLogin(t, true)
	page, pending, csrf, c := pl.password(t)
	require.Equal(t, http.StatusOK, page.Code)
	assert.Contains(t, page.Body.String(), `id="pk"`, "the second step offers the passkey")

	opts, session := pl.begin(t, pending, csrf, c)
	w := pl.finish(pending, csrf, session, pl.auth.Get(t, opts, principalID), c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var done struct {
		Redirect string `json:"redirect"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &done))
	loc, err := url.Parse(done.Redirect)
	require.NoError(t, err)
	require.NotEmpty(t, loc.Query().Get("code"))

	var sess *domain.AuthSession
	for _, s := range pl.store.sessions {
		sess = s
	}
	require.NotNil(t, sess, "the login session is recorded")
	assert.Contains(t, sess.AMR, domain.AMRHardwareKey)
	assert.Equal(t, domain.AssurancePhishingResistant, sess.AssuranceLevel)
	assert.Equal(t, "MFA_LOGIN", pl.keys.LastDecision().Action)
}

// A phished assertion is refused, and repeated refusals count against the
// login failure budget (the harness allows 3 per IP) until the step is
// throttled with Retry-After.
func TestAU11_HostedLoginRefusesAWrongPasskeyAndThrottles(t *testing.T) {
	pl := newPasskeyLogin(t, true)
	_, pending, csrf, c := pl.password(t)
	pl.auth.Origin = "https://phish.example"
	opts, session := pl.begin(t, pending, csrf, c)
	w := pl.finish(pending, csrf, session, pl.auth.Get(t, opts, principalID), c)
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "passkey_rejected")
	for i := 0; i < 5 && w.Code == http.StatusUnauthorized; i++ {
		opts, session = pl.begin(t, pending, csrf, c)
		w = pl.finish(pending, csrf, session, pl.auth.Get(t, opts, principalID), c)
	}
	assert.Equal(t, http.StatusTooManyRequests, w.Code, w.Body.String())
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Empty(t, pl.store.sessions, "no login session for a refused passkey")
}

func TestAU11_HostedLoginPasskeyStepIsOnlyForThoseOfferedIt(t *testing.T) {
	pl := newPasskeyLogin(t, false) // alice has no passkey: the password completes the login
	page, pending, csrf, c := pl.password(t)
	require.Equal(t, http.StatusFound, page.Code, page.Body.String())
	w := pl.finish(pending, csrf, "anything", json.RawMessage(`{}`), c)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
