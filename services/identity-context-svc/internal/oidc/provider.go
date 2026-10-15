package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/keys"
	"zoiko.io/identity-context-svc/internal/outbox"
	"zoiko.io/identity-context-svc/internal/ratelimit"
	"zoiko.io/identity-context-svc/internal/siem"
	idenv "zoiko.io/identity-envelope"
)

// Store is the provider's slice of the Postgres store.
type Store interface {
	CreateAuthSession(ctx context.Context, a domain.AuthSession, recs ...outbox.Record) error
	GetAuthSession(ctx context.Context, tenantID, sid string) (*domain.AuthSession, error)
	EndAuthSession(ctx context.Context, tenantID, sid, reason string, at time.Time, recs ...outbox.Record) ([]string, error)
	InsertAuthCode(ctx context.Context, c domain.AuthorizationCode) error
	ConsumeAuthCode(ctx context.Context, codeHash string, now time.Time) (*domain.AuthorizationCode, error)
	InsertRefreshToken(ctx context.Context, t domain.RefreshToken) error
	RotateRefreshToken(ctx context.Context, presentedHash string, now time.Time,
		check func(*domain.RefreshToken) error, next func(*domain.RefreshToken) domain.RefreshToken) (*domain.RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, tokenHash, clientID string, now time.Time) error
	RevokeAccessToken(ctx context.Context, tenantID, jti string, expiresAt time.Time) error
	AccessTokenRevoked(ctx context.Context, tenantID, jti string) (bool, error)
	FindByID(ctx context.Context, principalID, tenantID string) (*domain.Principal, error)
	FindByIDPSubject(ctx context.Context, subject, tenantID string) (*domain.Principal, error)
	FindSessionContext(ctx context.Context, sessionContextID, tenantID string) (*domain.SessionContext, error)
}

// PasswordVerifier checks a password (context.Authenticator).
type PasswordVerifier interface {
	VerifyPassword(ctx context.Context, tenantID, email, password, correlationID string) (*domain.Principal, error)
}

// SecondFactor is TOTP at login (context.MFAService).
type SecondFactor interface {
	HasActiveTOTP(ctx context.Context, principalID, tenantID string) (bool, error)
	VerifyLoginTOTP(ctx context.Context, p *domain.Principal, code, correlationID string) error
}

// Passkeys is WebAuthn at login (internal/webauthn). Optional.
type Passkeys interface {
	HasPasskey(ctx context.Context, tenantID, principalID string) (bool, error)
	BeginLogin(ctx context.Context, p *domain.Principal) (options json.RawMessage, sessionKey string, err error)
	FinishLogin(ctx context.Context, p *domain.Principal, sessionKey string, assertion []byte) error
}

// Federator starts an upstream login (internal/federation). Optional.
type Federator interface {
	ActiveIdPs(ctx context.Context, tenantID string) ([]domain.IdentityProvider, error)
	Start(w http.ResponseWriter, r *http.Request, idp *domain.IdentityProvider, pendingID string) error
}

// SessionInvalidator ends a session context with its events (the resolver).
type SessionInvalidator interface {
	InvalidateSession(ctx context.Context, sessionContextID, tenantID string, reason domain.InvalidationReason, actorPrincipalID, correlationID string) error
}

// Events emits governance events.
type Events interface {
	Emit(ctx context.Context, eventType, tenantID, actorID, correlationID, key string, payload map[string]any) error
}

// Options wires a Provider.
type Options struct {
	Cfg            *config.Config
	Clients        *Registry
	Store          Store
	Pending        PendingStore
	Ring           *keys.Keyring
	AccessTokens   *auth.IdPTokenIssuer
	AccessVerifier *auth.JWTVerifier
	Passwords      PasswordVerifier
	MFA            SecondFactor
	Passkeys       Passkeys
	Federation     Federator
	Sessions       SessionInvalidator
	Events         Events
	Limiter        *ratelimit.Limiter
	SIEM           *siem.Client
	Log            *zap.Logger
	// PeerName is the verified mTLS client-certificate name, "" otherwise.
	PeerName func(*http.Request) string
	Metrics  *Metrics
	Now      func() time.Time
}

// Provider is the OIDC provider.
type Provider struct {
	o      Options
	ap     config.AuthPlaneConfig
	issuer string
	dpop   *idenv.DPoPVerifier
}

// New builds a provider.
func New(o Options) (*Provider, error) {
	if o.Cfg == nil || o.Clients == nil || o.Store == nil || o.Pending == nil || o.Ring == nil ||
		o.AccessTokens == nil || o.AccessVerifier == nil || o.Passwords == nil || o.Sessions == nil {
		return nil, errors.New("oidc: incomplete wiring")
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.PeerName == nil {
		o.PeerName = idenv.VerifiedPeerName
	}
	if o.Metrics == nil {
		o.Metrics = NopMetrics()
	}
	d := idenv.NewDPoPVerifier()
	d.Replay = replayCache{store: o.Pending}
	return &Provider{o: o, ap: o.Cfg.AuthPlane, issuer: o.Cfg.AuthPlane.IssuerURL, dpop: d}, nil
}

// RegisterRoutes mounts the provider.
func (p *Provider) RegisterRoutes(r chi.Router) {
	r.Get("/.well-known/openid-configuration", p.Discovery)
	r.Get("/.well-known/oauth-authorization-server", p.Discovery)
	r.Get("/oauth2/authorize", p.Authorize)
	r.Post("/oauth2/authorize", p.Authorize)
	r.Post("/oauth2/login", p.LoginPassword)
	r.Post("/oauth2/login/mfa", p.LoginTOTP)
	r.Post("/oauth2/login/passkey/begin", p.LoginPasskeyBegin)
	r.Post("/oauth2/login/passkey/finish", p.LoginPasskeyFinish)
	r.Get("/oauth2/login/federate/{idpID}", p.LoginFederate)
	r.Post("/oauth2/token", p.Token)
	r.Get("/oauth2/userinfo", p.UserInfo)
	r.Post("/oauth2/userinfo", p.UserInfo)
	r.Post("/oauth2/revoke", p.Revoke)
	r.Post("/oauth2/introspect", p.Introspect)
	r.Get("/oauth2/logout", p.Logout)
	r.Post("/oauth2/logout", p.Logout)
	r.Post("/v1/logout", p.APILogout)
	r.Post("/v1/context/session/introspect", p.SessionStatus)
}

// IsProtocolPath reports whether path is an OAuth/OIDC protocol endpoint the
// §4 envelope contract and the idempotency middleware must not apply to:
// these are called by browsers and OAuth clients that speak RFC 6749, and
// their replay protection is the protocol's own (single-use codes, rotating
// refresh tokens, DPoP jti).
func IsProtocolPath(path string) bool {
	return strings.HasPrefix(path, "/oauth2/") || strings.HasPrefix(path, "/.well-known/") ||
		path == "/v1/context/session/introspect" || path == "/v1/logout"
}

// ── Errors (RFC 6749 §5.2 / OIDC Core §3.1.2.6) ─────────────────────────────

type oauthError struct {
	Status      int
	Code        string
	Description string
}

func (e *oauthError) Error() string { return e.Code + ": " + e.Description }

func errInvalidRequest(d string) *oauthError {
	return &oauthError{http.StatusBadRequest, "invalid_request", d}
}
func errInvalidClient(d string) *oauthError {
	return &oauthError{http.StatusUnauthorized, "invalid_client", d}
}
func errInvalidGrant(d string) *oauthError {
	return &oauthError{http.StatusBadRequest, "invalid_grant", d}
}
func errUnauthorizedClient(d string) *oauthError {
	return &oauthError{http.StatusBadRequest, "unauthorized_client", d}
}
func errInvalidTarget(d string) *oauthError {
	return &oauthError{http.StatusBadRequest, "invalid_target", d}
}
func errInvalidDPoP(d string) *oauthError {
	return &oauthError{http.StatusBadRequest, "invalid_dpop_proof", d}
}
func errServer(d string) *oauthError {
	return &oauthError{http.StatusServiceUnavailable, "temporarily_unavailable", d}
}

func writeOAuthError(w http.ResponseWriter, e *oauthError) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if e.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="identity-context-svc"`)
	}
	w.WriteHeader(e.Status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": e.Code, "error_description": e.Description})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ── Client authentication (RFC 6749 §2.3, RFC 8705 §2) ──────────────────────

// authenticateClient identifies the client of a token-endpoint style request.
// A confidential or workload client must prove a secret or its certificate;
// a public client only names itself (and is then bound by PKCE/DPoP).
func (p *Provider) authenticateClient(r *http.Request) (*Client, *oauthError) {
	id, secret, basic := r.BasicAuth()
	if basic {
		var err error
		if id, err = url.QueryUnescape(id); err != nil {
			return nil, errInvalidClient("malformed client_id")
		}
		if secret, err = url.QueryUnescape(secret); err != nil {
			return nil, errInvalidClient("malformed client_secret")
		}
	} else {
		id = r.PostFormValue("client_id")
		secret = r.PostFormValue("client_secret")
	}
	peer := p.o.PeerName(r)
	if id == "" && peer != "" {
		// tls_client_auth with the client identified by its certificate.
		for _, cand := range []string{peer} {
			if c, ok := p.o.Clients.Get(cand); ok && c.TLSClientAuthSubject == peer {
				return c, nil
			}
		}
	}
	if id == "" {
		return nil, errInvalidClient("client authentication is required")
	}
	c, ok := p.o.Clients.Get(id)
	if !ok {
		p.o.Metrics.ClientAuthFailure()
		return nil, errInvalidClient("unknown client")
	}
	switch {
	case c.TLSClientAuthSubject != "" && peer != "" && subtle.ConstantTimeCompare([]byte(peer), []byte(c.TLSClientAuthSubject)) == 1:
		return c, nil
	case c.ClientType == ClientPublic:
		if secret != "" {
			return nil, errInvalidClient("a public client has no secret")
		}
		return c, nil
	case c.CheckSecret(secret):
		return c, nil
	default:
		p.o.Metrics.ClientAuthFailure()
		return nil, errInvalidClient("client authentication failed")
	}
}

func (p *Provider) correlation(r *http.Request) string {
	if c := strings.TrimSpace(r.Header.Get("X-Correlation-ID")); c != "" {
		return c
	}
	return idenv.RequestIDFrom(r)
}

func (p *Provider) clientIP(r *http.Request) string {
	return ratelimit.ClientIP(r, p.ap.TrustedProxyCIDRs)
}

func (p *Provider) emit(ctx context.Context, eventType, tenantID, actor, corr, key string, payload map[string]any) {
	if p.o.Events == nil {
		return
	}
	if err := p.o.Events.Emit(ctx, eventType, tenantID, actor, corr, key, payload); err != nil {
		p.o.Log.Error("event publish failed", zap.String("event_type", eventType), zap.Error(err))
	}
}

func (p *Provider) stream(ctx context.Context, tenantID, eventType string, sev siem.Severity, msg string) {
	if p.o.SIEM != nil {
		p.o.SIEM.Stream(ctx, tenantID, eventType, sev, msg)
	}
}

// endLoginSession ends sid everywhere: the auth session row, its refresh
// tokens, and every session context resolved under it (each then answers
// inactive at every relying party's revocation check).
func (p *Provider) endLoginSession(ctx context.Context, tenantID, sid, reason, actor, corr string) (int, error) {
	if sid == "" {
		return 0, nil
	}
	now := p.o.Now()
	ids, err := p.o.Store.EndAuthSession(ctx, tenantID, sid, reason, now)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	invReason := domain.InvalidationReasonLogout
	if reason != domain.SessionEndLogout {
		invReason = domain.InvalidationReasonAdminRevoke
	}
	n := 0
	for _, id := range ids {
		if err := p.o.Sessions.InvalidateSession(ctx, id, tenantID, invReason, actor, corr); err != nil {
			p.o.Log.Error("session context not invalidated with its login session",
				zap.String("sid", sid), zap.String("session_context_id", id), zap.Error(err))
			continue
		}
		n++
	}
	p.emit(ctx, "identity.login_session.ended", tenantID, actor, corr, sid, map[string]any{
		"sid": sid, "tenant_id": tenantID, "reason": reason, "session_contexts_ended": n,
	})
	p.o.Metrics.LoginSessionEnded(reason)
	return n, nil
}
