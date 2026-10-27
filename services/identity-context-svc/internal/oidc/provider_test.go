package oidc_test

// Tests for the OIDC provider, which had none when it was wired into
// cmd/server on 19 Oct 2026 (Group 1 re-audit F-5). They drive the real
// router, keyring, token issuer and verifier against an in-memory store, so
// the protocol rules (PKCE, exact redirects, single-use codes, refresh
// rotation and reuse detection, client authentication, session status) are
// checked end to end without a database. The Postgres half of the same store
// interface is covered by internal/store's OAuth tests.

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/keys"
	"zoiko.io/identity-context-svc/internal/oidc"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/ratelimit"
)

// ── fakes ────────────────────────────────────────────────────────────────────

type memStore struct {
	mu         sync.Mutex
	sessions   map[string]*domain.AuthSession // tenant/sid
	codes      map[string]*domain.AuthorizationCode
	refresh    map[string]*domain.RefreshToken
	revokedJTI map[string]bool
	principals map[string]*domain.Principal // principal id
	contexts   map[string]*domain.SessionContext
}

func newMemStore() *memStore {
	return &memStore{sessions: map[string]*domain.AuthSession{}, codes: map[string]*domain.AuthorizationCode{},
		refresh: map[string]*domain.RefreshToken{}, revokedJTI: map[string]bool{},
		principals: map[string]*domain.Principal{}, contexts: map[string]*domain.SessionContext{}}
}

func (m *memStore) CreateAuthSession(_ context.Context, a domain.AuthSession, _ ...outbox.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := a
	m.sessions[a.TenantID+"/"+a.SID] = &cp
	return nil
}

func (m *memStore) GetAuthSession(_ context.Context, tenantID, sid string) (*domain.AuthSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[tenantID+"/"+sid]
	if s == nil {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (m *memStore) EndAuthSession(_ context.Context, tenantID, sid, reason string, at time.Time, _ ...outbox.Record) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[tenantID+"/"+sid]
	if s == nil {
		return nil, nil
	}
	if s.EndedAt == nil {
		s.EndedAt, s.EndReason = &at, reason
	}
	for _, t := range m.refresh {
		if t.SID == sid && t.RevokedAt == nil {
			t.RevokedAt, t.RevokeReason = &at, reason
		}
	}
	var ids []string
	for id, sc := range m.contexts {
		if sc.AuthSessionID == sid {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (m *memStore) InsertAuthCode(_ context.Context, c domain.AuthorizationCode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := c
	m.codes[c.CodeHash] = &cp
	return nil
}

func (m *memStore) ConsumeAuthCode(_ context.Context, codeHash string, now time.Time) (*domain.AuthorizationCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.codes[codeHash]
	if c == nil {
		return nil, domain.ErrCodeInvalid
	}
	cp := *c
	if c.ConsumedAt != nil {
		return &cp, domain.ErrCodeReplayed
	}
	if !now.Before(c.ExpiresAt) {
		return nil, domain.ErrCodeInvalid
	}
	c.ConsumedAt = &now
	return &cp, nil
}

func (m *memStore) InsertRefreshToken(_ context.Context, t domain.RefreshToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := t
	m.refresh[t.TokenHash] = &cp
	return nil
}

func (m *memStore) RotateRefreshToken(_ context.Context, presented string, now time.Time,
	check func(*domain.RefreshToken) error, next func(*domain.RefreshToken) domain.RefreshToken) (*domain.RefreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.refresh[presented]
	if t == nil {
		return nil, domain.ErrRefreshInvalid
	}
	cp := *t
	if t.UsedAt != nil {
		for _, o := range m.refresh {
			if o.FamilyID == t.FamilyID && o.RevokedAt == nil {
				o.RevokedAt, o.RevokeReason = &now, domain.SessionEndRefreshReuse
			}
		}
		if s := m.sessions[t.TenantID+"/"+t.SID]; s != nil && s.EndedAt == nil {
			s.EndedAt, s.EndReason = &now, domain.SessionEndRefreshReuse
		}
		return &cp, domain.ErrRefreshReused
	}
	if t.RevokedAt != nil || !now.Before(t.ExpiresAt) {
		return nil, domain.ErrRefreshInvalid
	}
	if check != nil {
		if err := check(&cp); err != nil {
			return nil, err
		}
	}
	t.UsedAt = &now
	n := next(&cp)
	m.refresh[n.TokenHash] = &n
	return &cp, nil
}

func (m *memStore) RevokeRefreshToken(_ context.Context, tokenHash, clientID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.refresh[tokenHash]; t != nil && t.ClientID == clientID {
		for _, o := range m.refresh {
			if o.FamilyID == t.FamilyID && o.RevokedAt == nil {
				o.RevokedAt = &now
			}
		}
	}
	return nil
}

func (m *memStore) RevokeAccessToken(_ context.Context, tenantID, jti string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedJTI[tenantID+"/"+jti] = true
	return nil
}

func (m *memStore) AccessTokenRevoked(_ context.Context, tenantID, jti string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokedJTI[tenantID+"/"+jti], nil
}

func (m *memStore) FindByID(_ context.Context, principalID, tenantID string) (*domain.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.principals[principalID]
	if p == nil || p.TenantID != tenantID {
		return nil, nil
	}
	cp := *p
	return &cp, nil
}

func (m *memStore) FindByIDPSubject(_ context.Context, subject, tenantID string) (*domain.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.principals {
		if p.IdentityProviderSubject == subject && p.TenantID == tenantID {
			cp := *p
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memStore) FindSessionContext(_ context.Context, id, tenantID string) (*domain.SessionContext, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc := m.contexts[id]
	if sc == nil || sc.TenantID != tenantID {
		return nil, nil
	}
	cp := *sc
	return &cp, nil
}

func (m *memStore) sessionFor(tenantID, sid string) *domain.AuthSession {
	s, _ := m.GetAuthSession(context.Background(), tenantID, sid)
	return s
}

type passwords struct{ store *memStore }

var errBadCredentials = errors.New("invalid credentials")

func (p passwords) VerifyPassword(_ context.Context, tenantID, email, password, _ string) (*domain.Principal, error) {
	p.store.mu.Lock()
	defer p.store.mu.Unlock()
	for _, pr := range p.store.principals {
		if pr.TenantID == tenantID && pr.Email == email && password == "correct horse battery" && pr.Status == domain.PrincipalStatusActive {
			cp := *pr
			return &cp, nil
		}
	}
	return nil, errBadCredentials
}

type sessionInvalidator struct{ store *memStore }

func (s sessionInvalidator) InvalidateSession(_ context.Context, id, tenantID string, reason domain.InvalidationReason, _, _ string) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if sc := s.store.contexts[id]; sc != nil && sc.TenantID == tenantID && sc.InvalidatedAt == nil {
		now := time.Now().UTC()
		r := reason
		sc.InvalidatedAt, sc.InvalidationReason = &now, &r
	}
	return nil
}

type recordedEvents struct {
	mu    sync.Mutex
	types []string
}

func (e *recordedEvents) Emit(_ context.Context, eventType, _, _, _, _ string, _ map[string]any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.types = append(e.types, eventType)
	return nil
}

func (e *recordedEvents) has(t string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, x := range e.types {
		if x == t {
			return true
		}
	}
	return false
}

// ── harness ──────────────────────────────────────────────────────────────────

const (
	issuer      = "http://idp.test"
	redirect    = "https://app.test/cb"
	tenant      = "t1"
	principalID = "p-alice"
	secret      = "console-secret"
)

type harness struct {
	t        *testing.T
	store    *memStore
	events   *recordedEvents
	ring     *keys.Keyring
	cfg      *config.Config
	verifier *auth.JWTVerifier
	router   http.Handler
	provider *oidc.Provider
}

func sha(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func newHarness(t *testing.T) *harness {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := keys.NewPEMSigner("k1", pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
	require.NoError(t, err)
	ring, err := keys.NewKeyring(signer)
	require.NoError(t, err)

	cfg := &config.Config{JWTIssuer: "identity-context-svc", IdPTokenTTLSeconds: 300, EnvelopeJWTTTLSeconds: 300,
		AuthPlane: config.AuthPlaneConfig{IssuerURL: issuer, CodeTTL: time.Minute, IDTokenTTL: 5 * time.Minute,
			RefreshTTL: 8 * time.Hour, RefreshIdleTTL: time.Hour, LoginSessionMax: 12 * time.Hour, LoginMFA: "enrolled",
			ExchangeTTL: 5 * time.Minute}}
	issuerTok, err := auth.NewIdPTokenIssuer(cfg, ring)
	require.NoError(t, err)
	verifier := auth.NewJWTVerifier(cfg, ring)

	reg, err := oidc.NewRegistry(
		oidc.Client{ClientID: "spa", ClientType: oidc.ClientPublic, RedirectURIs: []string{redirect},
			GrantTypes: []string{oidc.GrantAuthorizationCode, oidc.GrantRefreshToken}},
		oidc.Client{ClientID: "console", ClientType: oidc.ClientConfidential, SecretSHA256: sha(secret),
			RedirectURIs: []string{redirect}, GrantTypes: []string{oidc.GrantAuthorizationCode, oidc.GrantRefreshToken}},
		oidc.Client{ClientID: "worker", ClientType: oidc.ClientWorkload, SecretSHA256: sha(secret),
			GrantTypes: []string{oidc.GrantClientCredentials}, AllowedAudiences: []string{"policy-svc"}},
	)
	require.NoError(t, err)

	st := newMemStore()
	st.principals[principalID] = &domain.Principal{PrincipalID: principalID, TenantID: tenant, Email: "alice@corp.test",
		DisplayName: "Alice", PrincipalType: domain.PrincipalTypeHuman, Status: domain.PrincipalStatusActive,
		IdentityProviderSubject: "local:" + principalID}
	ev := &recordedEvents{}
	limiter := ratelimit.New(ratelimit.NewMemoryCounter(), ratelimit.Limits{AttemptsPerIPPerMinute: 100,
		FailuresPerIP: 3, FailuresPerTenant: 100, FailureWindow: 10 * time.Minute}, nil)

	p, err := oidc.New(oidc.Options{Cfg: cfg, Clients: reg, Store: st, Pending: oidc.NewMemoryPending(), Ring: ring,
		AccessTokens: issuerTok, AccessVerifier: verifier, Passwords: passwords{st}, Sessions: sessionInvalidator{st},
		Events: ev, Limiter: limiter, PeerName: func(*http.Request) string { return "" }})
	require.NoError(t, err)
	r := chi.NewRouter()
	p.RegisterRoutes(r)
	return &harness{t: t, store: st, events: ev, ring: ring, cfg: cfg, verifier: verifier, router: r, provider: p}
}

func (h *harness) do(req *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func form(method, target string, v url.Values, cookies ...*http.Cookie) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	return req
}

func cookie(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

var (
	pendingRe = regexp.MustCompile(`name="pending" value="([^"]+)"`)
	csrfRe    = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
)

const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXkdBjftJeZ4CVP"

func challenge(v string) string {
	sum := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeQuery(client, scope string) url.Values {
	return url.Values{"client_id": {client}, "redirect_uri": {redirect}, "response_type": {"code"},
		"scope": {scope}, "state": {"st-1"}, "nonce": {"n-1"}, "code_challenge": {challenge(verifier)},
		"code_challenge_method": {"S256"}, "tenant": {tenant}}
}

// login runs authorize + hosted password login and returns the code.
func (h *harness) login(client, scope string) string {
	h.t.Helper()
	w := h.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+authorizeQuery(client, scope).Encode(), nil))
	require.Equal(h.t, http.StatusOK, w.Code, w.Body.String())
	pend, csrf := pendingRe.FindStringSubmatch(w.Body.String()), csrfRe.FindStringSubmatch(w.Body.String())
	require.Len(h.t, pend, 2, "login page carries the pending id")
	require.Len(h.t, csrf, 2, "login page carries the CSRF token")

	w = h.do(form(http.MethodPost, "/oauth2/login", url.Values{"pending": {pend[1]}, "csrf": {csrf[1]}, "tenant": {tenant},
		"email": {"alice@corp.test"}, "password": {"correct horse battery"}}, cookie(w, "zoiko_login_csrf")))
	require.Equal(h.t, http.StatusFound, w.Code, w.Body.String())
	loc, err := url.Parse(w.Header().Get("Location"))
	require.NoError(h.t, err)
	require.Equal(h.t, "app.test", loc.Host)
	require.Equal(h.t, "st-1", loc.Query().Get("state"))
	require.Equal(h.t, issuer, loc.Query().Get("iss"), "RFC 9207 iss on the authorization response")
	require.NotEmpty(h.t, loc.Query().Get("code"))
	return loc.Query().Get("code")
}

func (h *harness) redeem(code, client, codeVerifier string, extra ...string) (*httptest.ResponseRecorder, map[string]any) {
	v := url.Values{"grant_type": {oidc.GrantAuthorizationCode}, "code": {code}, "code_verifier": {codeVerifier},
		"redirect_uri": {redirect}, "client_id": {client}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	w := h.do(form(http.MethodPost, "/oauth2/token", v))
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

// ── tests ────────────────────────────────────────────────────────────────────

// Every route the provider mounts is in openapi.yaml, and every OIDC path in
// openapi.yaml is mounted (the provider half of internal/context's contract
// test, which lists these routes as mounted elsewhere).
func TestEveryProviderRouteIsPublished(t *testing.T) {
	h := newHarness(t)
	served := map[string]bool{}
	require.NoError(t, chi.Walk(h.router.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.ToLower(route)] = true
		return nil
	}))
	require.GreaterOrEqual(t, len(served), 15)

	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err)
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	published := map[string]bool{}
	for path, methods := range doc.Paths {
		if !oidc.IsProtocolPath(path) || path == "/.well-known/jwks.json" {
			continue
		}
		for m := range methods {
			published[strings.ToUpper(m)+" "+strings.ToLower(path)] = true
		}
	}
	var missing, extra []string
	for k := range served {
		if !published[k] {
			missing = append(missing, k)
		}
	}
	for k := range published {
		if !served[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	assert.Empty(t, missing, "served but not in openapi.yaml")
	assert.Empty(t, extra, "in openapi.yaml but not served")
}

func TestDiscoveryAdvertisesTheCodeFlowWithPKCE(t *testing.T) {
	h := newHarness(t)
	w := h.do(httptest.NewRequest(http.MethodGet, "/.well-known/openid-configuration", nil))
	require.Equal(t, http.StatusOK, w.Code)
	var d map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &d))
	assert.Equal(t, issuer, d["issuer"])
	assert.Equal(t, issuer+"/oauth2/token", d["token_endpoint"])
	assert.Equal(t, []any{"code"}, d["response_types_supported"])
	assert.Equal(t, []any{"S256"}, d["code_challenge_methods_supported"])
}

// An unknown client or an unregistered redirect is answered on the page, never
// redirected (RFC 9700 §4.1: no open redirector).
func TestAuthorizeNeverRedirectsToAnUnregisteredTarget(t *testing.T) {
	h := newHarness(t)
	q := authorizeQuery("nobody", "openid")
	w := h.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, w.Header().Get("Location"))

	q = authorizeQuery("spa", "openid")
	q.Set("redirect_uri", "https://evil.test/cb")
	w = h.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+q.Encode(), nil))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, w.Header().Get("Location"))
}

func TestAuthorizeRequiresPKCES256AndTheCodeFlow(t *testing.T) {
	h := newHarness(t)
	for name, mutate := range map[string]func(url.Values){
		"no challenge":    func(q url.Values) { q.Del("code_challenge") },
		"plain challenge": func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"implicit flow":   func(q url.Values) { q.Set("response_type", "token") },
		"no openid scope": func(q url.Values) { q.Set("scope", "profile") },
		"request object":  func(q url.Values) { q.Set("request", "eyJ...") },
	} {
		t.Run(name, func(t *testing.T) {
			q := authorizeQuery("spa", "openid")
			mutate(q)
			w := h.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+q.Encode(), nil))
			require.Equal(t, http.StatusFound, w.Code)
			loc, _ := url.Parse(w.Header().Get("Location"))
			assert.Equal(t, "app.test", loc.Host)
			assert.NotEmpty(t, loc.Query().Get("error"))
			assert.Empty(t, loc.Query().Get("code"))
		})
	}
}

// The full flow: hosted login → code → tokens bound to the login session.
func TestCodeFlowIssuesTokensBoundToTheLoginSession(t *testing.T) {
	h := newHarness(t)
	code := h.login("spa", "openid offline_access")
	w, body := h.redeem(code, "spa", verifier)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	require.NotEmpty(t, body["access_token"])
	require.NotEmpty(t, body["id_token"])
	require.NotEmpty(t, body["refresh_token"], "offline_access yields a refresh token")

	vc, err := h.verifier.VerifyBearer(context.Background(), body["access_token"].(string))
	require.NoError(t, err)
	assert.Equal(t, "local:"+principalID, vc.Subject)
	assert.Equal(t, tenant, vc.TenantID)
	assert.Equal(t, "spa", vc.ClientID)
	require.NotEmpty(t, vc.SID)
	assert.True(t, h.store.sessionFor(tenant, vc.SID).Live(time.Now()))
	assert.True(t, h.events.has("identity.login_session.started"))
}

// A code is single use; a second redemption means it leaked, so the login
// session it came from is ended (RFC 9700 §4.5.1).
func TestCodeReplayEndsTheLoginSession(t *testing.T) {
	h := newHarness(t)
	code := h.login("spa", "openid")
	w, body := h.redeem(code, "spa", verifier)
	require.Equal(t, http.StatusOK, w.Code)
	vc, err := h.verifier.VerifyBearer(context.Background(), body["access_token"].(string))
	require.NoError(t, err)

	w, body = h.redeem(code, "spa", verifier)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_grant", body["error"])
	sess := h.store.sessionFor(tenant, vc.SID)
	require.NotNil(t, sess.EndedAt, "the leaked code's login session is ended")
	assert.Equal(t, domain.SessionEndCodeReuse, sess.EndReason)
	assert.True(t, h.events.has("identity.authorization_code.replayed"))
}

func TestCodeRedemptionChecksVerifierRedirectAndClient(t *testing.T) {
	h := newHarness(t)

	w, body := h.redeem(h.login("spa", "openid"), "spa", strings.Repeat("x", 43))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_grant", body["error"], "wrong PKCE verifier")

	w, body = h.redeem(h.login("spa", "openid"), "spa", verifier, "redirect_uri", "https://app.test/other")
	assert.Equal(t, "invalid_grant", body["error"], "redirect_uri must match the authorization request")

	// A code issued to the public client cannot be redeemed by another, and
	// a confidential client must prove its secret.
	code := h.login("spa", "openid")
	w, body = h.redeem(code, "console", verifier)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "invalid_client", body["error"])
	w, body = h.redeem(code, "console", verifier, "client_secret", secret)
	assert.Equal(t, "invalid_grant", body["error"], "the code was issued to another client")
}

// Refresh tokens rotate; presenting a rotated one revokes its family and ends
// the login session.
func TestRefreshRotatesAndReuseEndsTheSession(t *testing.T) {
	h := newHarness(t)
	_, body := h.redeem(h.login("spa", "openid offline_access"), "spa", verifier)
	first := body["refresh_token"].(string)
	vc, err := h.verifier.VerifyBearer(context.Background(), body["access_token"].(string))
	require.NoError(t, err)

	refresh := func(tok string) (*httptest.ResponseRecorder, map[string]any) {
		w := h.do(form(http.MethodPost, "/oauth2/token", url.Values{"grant_type": {oidc.GrantRefreshToken},
			"refresh_token": {tok}, "client_id": {"spa"}}))
		var b map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &b)
		return w, b
	}
	w, b := refresh(first)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	second := b["refresh_token"].(string)
	require.NotEqual(t, first, second, "the refresh token rotates")

	w, b = refresh(first)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_grant", b["error"])
	assert.NotNil(t, h.store.sessionFor(tenant, vc.SID).EndedAt, "reuse ends the login session")
	assert.True(t, h.events.has("identity.refresh_token.reuse_detected"))

	w, b = refresh(second)
	assert.Equal(t, "invalid_grant", b["error"], "the whole family is revoked")
}

// Password spraying from one source is throttled (AU-04).
func TestHostedLoginIsThrottledAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t)
	var last *httptest.ResponseRecorder
	for i := 0; i < 5; i++ {
		w := h.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+authorizeQuery("spa", "openid").Encode(), nil))
		pend, csrf := pendingRe.FindStringSubmatch(w.Body.String()), csrfRe.FindStringSubmatch(w.Body.String())
		last = h.do(form(http.MethodPost, "/oauth2/login", url.Values{"pending": {pend[1]}, "csrf": {csrf[1]},
			"tenant": {tenant}, "email": {"alice@corp.test"}, "password": {"wrong"}}, cookie(w, "zoiko_login_csrf")))
	}
	assert.Equal(t, http.StatusTooManyRequests, last.Code)
	assert.NotEmpty(t, last.Header().Get("Retry-After"))
}

// The login form is bound to its CSRF cookie.
func TestHostedLoginRefusesAFormWithoutItsCSRFCookie(t *testing.T) {
	h := newHarness(t)
	w := h.do(httptest.NewRequest(http.MethodGet, "/oauth2/authorize?"+authorizeQuery("spa", "openid").Encode(), nil))
	pend, csrf := pendingRe.FindStringSubmatch(w.Body.String()), csrfRe.FindStringSubmatch(w.Body.String())
	w = h.do(form(http.MethodPost, "/oauth2/login", url.Values{"pending": {pend[1]}, "csrf": {csrf[1]},
		"tenant": {tenant}, "email": {"alice@corp.test"}, "password": {"correct horse battery"}}))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestIntrospectionIsForConfidentialClientsOnly(t *testing.T) {
	h := newHarness(t)
	_, body := h.redeem(h.login("spa", "openid"), "spa", verifier)
	at := body["access_token"].(string)

	w := h.do(form(http.MethodPost, "/oauth2/introspect", url.Values{"token": {at}, "client_id": {"spa"}}))
	assert.Equal(t, http.StatusBadRequest, w.Code, "a public client may not introspect")

	w = h.do(form(http.MethodPost, "/oauth2/introspect", url.Values{"token": {at}, "client_id": {"console"}, "client_secret": {secret}}))
	require.Equal(t, http.StatusOK, w.Code)
	var b map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	assert.Equal(t, true, b["active"])

	// Revoked access tokens introspect inactive.
	w = h.do(form(http.MethodPost, "/oauth2/revoke", url.Values{"token": {at}, "client_id": {"spa"}}))
	require.Equal(t, http.StatusOK, w.Code)
	w = h.do(form(http.MethodPost, "/oauth2/introspect", url.Values{"token": {at}, "client_id": {"console"}, "client_secret": {secret}}))
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	assert.Equal(t, false, b["active"])
}

// Relying parties ask /v1/context/session/introspect whether an envelope's
// session is still live (AU-01). A logout through the provider ends it.
func TestSessionStatusFollowsLogout(t *testing.T) {
	h := newHarness(t)
	_, body := h.redeem(h.login("spa", "openid"), "spa", verifier)
	vc, err := h.verifier.VerifyBearer(context.Background(), body["access_token"].(string))
	require.NoError(t, err)

	now := time.Now().UTC()
	h.store.contexts["sc-1"] = &domain.SessionContext{SessionContextID: "sc-1", PrincipalID: principalID, TenantID: tenant,
		AuthSessionID: vc.SID, ExpiresAt: now.Add(5 * time.Minute)}
	env, err := h.ring.Sign(&domain.IdentityContextEnvelope{JTI: "j-1", ISS: "identity-context-svc", AUD: "zoiko-internal",
		IAT: now.Unix(), EXP: now.Add(5 * time.Minute).Unix(), TenantID: tenant,
		Principal:           domain.PrincipalClaims{PrincipalID: principalID, TenantID: tenant},
		SessionTrustPosture: domain.SessionTrustClaims{SessionContextID: "sc-1"}}, "JWT")
	require.NoError(t, err)

	status := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/v1/context/session/introspect", strings.NewReader(`{"token":"`+env+`"}`))
		w := h.do(req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var b map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
		return b
	}
	assert.Equal(t, true, status()["active"])

	w := h.do(form(http.MethodPost, "/oauth2/logout", url.Values{"id_token_hint": {body["id_token"].(string)}}))
	require.Equal(t, http.StatusOK, w.Code)
	st := status()
	assert.Equal(t, false, st["active"])
	assert.NotNil(t, h.store.contexts["sc-1"].InvalidatedAt, "the session context resolved under the login session is invalidated")
}

// Client credentials are for workload clients only, and never yield a
// person's identity.
func TestClientCredentialsAreForWorkloadsOnly(t *testing.T) {
	h := newHarness(t)
	w := h.do(form(http.MethodPost, "/oauth2/token", url.Values{"grant_type": {oidc.GrantClientCredentials},
		"client_id": {"console"}, "client_secret": {secret}, "audience": {"policy-svc"}}))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = h.do(form(http.MethodPost, "/oauth2/token", url.Values{"grant_type": {oidc.GrantClientCredentials},
		"client_id": {"worker"}, "client_secret": {secret}, "audience": {"policy-svc"}}))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var b map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	env, err := auth.VerifyAnyAudience(h.ring, "identity-context-svc", b["access_token"].(string))
	require.NoError(t, err)
	assert.Equal(t, "workload:worker", env.Principal.PrincipalID)
	assert.Equal(t, "policy-svc", env.AUD)

	w = h.do(form(http.MethodPost, "/oauth2/token", url.Values{"grant_type": {oidc.GrantClientCredentials},
		"client_id": {"worker"}, "client_secret": {secret}, "audience": {"vault-svc"}}))
	assert.Equal(t, http.StatusBadRequest, w.Code, "an audience the client is not registered for")
}
