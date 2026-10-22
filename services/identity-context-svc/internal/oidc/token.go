package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/siem"
	idenv "zoiko.io/identity-envelope"
)

const envelopeTokenType = "urn:zoikosuite:params:oauth:token-type:identity-envelope"

// tokenResponse is RFC 6749 §5.1 plus OIDC's id_token and RFC 8693's
// issued_token_type.
type tokenResponse struct {
	AccessToken     string `json:"access_token"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int    `json:"expires_in"`
	RefreshToken    string `json:"refresh_token,omitempty"`
	IDToken         string `json:"id_token,omitempty"`
	Scope           string `json:"scope,omitempty"`
	IssuedTokenType string `json:"issued_token_type,omitempty"`
}

// Token is the token endpoint (RFC 6749 §3.2).
func (p *Provider) Token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, errInvalidRequest("malformed form body"))
		return
	}
	if len(r.URL.Query()) > 0 && r.URL.Query().Get("code") != "" {
		writeOAuthError(w, errInvalidRequest("credentials must not travel in the URL"))
		return
	}
	ip := p.clientIP(r)
	if d := p.o.Limiter.Attempt(r.Context(), "token", ip, ""); !d.Allowed {
		p.o.Metrics.Throttled(d.Reason)
		w.Header().Set("Retry-After", d.RetryAfterSeconds())
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "slow_down", "error_description": "too many token requests"})
		return
	}
	c, oe := p.authenticateClient(r)
	if oe != nil {
		p.o.Limiter.Failure(r.Context(), ip, "")
		writeOAuthError(w, oe)
		return
	}
	grant := r.PostFormValue("grant_type")
	if !c.Allows(grant) {
		writeOAuthError(w, errUnauthorizedClient("this client may not use grant_type "+grant))
		return
	}
	var resp *tokenResponse
	switch grant {
	case GrantAuthorizationCode:
		resp, oe = p.grantCode(r, c)
	case GrantRefreshToken:
		resp, oe = p.grantRefresh(r, c)
	case GrantClientCredentials:
		resp, oe = p.grantClientCredentials(r, c)
	case GrantTokenExchange:
		resp, oe = p.grantExchange(r, c)
	default:
		oe = &oauthError{http.StatusBadRequest, "unsupported_grant_type", "grant_type " + grant + " is not supported"}
	}
	if oe != nil {
		writeOAuthError(w, oe)
		return
	}
	p.o.Metrics.Issued(grant)
	writeJSON(w, http.StatusOK, resp)
}

// dpopBinding verifies a DPoP proof on a token request and returns its key
// thumbprint ("" when no proof was sent and none is required).
func (p *Provider) dpopBinding(r *http.Request, c *Client) (string, *oauthError) {
	proof := r.Header.Get(idenv.HeaderDPoP)
	if proof == "" {
		if c.DPoPBound {
			return "", errInvalidDPoP("this client must present a DPoP proof (RFC 9449)")
		}
		return "", nil
	}
	pf, err := p.dpop.Verify(proof, http.MethodPost, p.issuer+"/oauth2/token", "")
	if err != nil {
		return "", errInvalidDPoP(err.Error())
	}
	return pf.JKT, nil
}

func (p *Provider) grantCode(r *http.Request, c *Client) (*tokenResponse, *oauthError) {
	ctx := r.Context()
	code, verifier := r.PostFormValue("code"), r.PostFormValue("code_verifier")
	if code == "" || verifier == "" {
		return nil, errInvalidRequest("code and code_verifier are required")
	}
	if !validVerifier(verifier) {
		return nil, errInvalidGrant("code_verifier is malformed (RFC 7636 §4.1)")
	}
	jkt, oe := p.dpopBinding(r, c)
	if oe != nil {
		return nil, oe
	}
	ac, err := p.o.Store.ConsumeAuthCode(ctx, HashToken(code), p.o.Now())
	if errors.Is(err, domain.ErrCodeReplayed) {
		// A second redemption means the code leaked. Everything issued from
		// it goes (RFC 6749 §4.1.2, RFC 9700 §4.5.1).
		p.o.Metrics.CodeReplay()
		corr := p.correlation(r)
		_, _ = p.endLoginSession(ctx, ac.TenantID, ac.SID, domain.SessionEndCodeReuse, "system", corr)
		p.emit(ctx, "identity.authorization_code.replayed", ac.TenantID, ac.PrincipalID, corr, ac.SID,
			map[string]any{"client_id": c.ClientID, "sid": ac.SID, "principal_id": ac.PrincipalID})
		p.stream(ctx, ac.TenantID, "identity.authorization_code.replayed", siem.SeverityHigh,
			"Authorization code presented twice for client "+c.ClientID+"; login session ended")
		return nil, errInvalidGrant("authorization code already used")
	}
	if err != nil {
		if errors.Is(err, domain.ErrCodeInvalid) {
			return nil, errInvalidGrant("authorization code invalid or expired")
		}
		p.o.Log.Error("code redemption failed", zap.Error(err))
		return nil, errServer("code redemption unavailable")
	}
	if ac.ClientID != c.ClientID {
		return nil, errInvalidGrant("the code was issued to another client")
	}
	if ac.RedirectURI != r.PostFormValue("redirect_uri") {
		return nil, errInvalidGrant("redirect_uri does not match the authorization request")
	}
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(ac.CodeChallenge)) != 1 {
		return nil, errInvalidGrant("PKCE verification failed")
	}
	if ac.DPoPJKT != "" && ac.DPoPJKT != jkt {
		return nil, errInvalidDPoP("the DPoP key does not match the dpop_jkt of the authorization request")
	}
	sess, pr, oe := p.liveSession(ctx, ac.TenantID, ac.SID)
	if oe != nil {
		return nil, oe
	}
	return p.issueTokens(ctx, c, pr, sess, ac.Scope, ac.Nonce, jkt, "")
}

func (p *Provider) grantRefresh(r *http.Request, c *Client) (*tokenResponse, *oauthError) {
	ctx := r.Context()
	presented := r.PostFormValue("refresh_token")
	if presented == "" {
		return nil, errInvalidRequest("refresh_token is required")
	}
	jkt, oe := p.dpopBinding(r, c)
	if oe != nil {
		return nil, oe
	}
	requested := normalizeScope(r.PostFormValue("scope"))
	now := p.o.Now()
	newToken := RandomToken(32)
	var bindErr *oauthError
	old, err := p.o.Store.RotateRefreshToken(ctx, HashToken(presented), now,
		func(t *domain.RefreshToken) error {
			if t.ClientID != c.ClientID {
				bindErr = errInvalidGrant("the refresh token was issued to another client")
				return errors.New("client mismatch")
			}
			if t.DPoPJKT != "" && t.DPoPJKT != jkt {
				bindErr = errInvalidDPoP("the refresh token is bound to another DPoP key")
				return errors.New("dpop mismatch")
			}
			if requested != "" {
				for _, s := range strings.Fields(requested) {
					if !hasScope(t.Scope, s) {
						bindErr = &oauthError{http.StatusBadRequest, "invalid_scope", "a refresh cannot widen the scope"}
						return errors.New("scope widening")
					}
				}
			}
			return nil
		},
		func(t *domain.RefreshToken) domain.RefreshToken {
			exp := now.Add(p.ap.RefreshIdleTTL)
			if exp.After(t.FamilyExpiresAt) {
				exp = t.FamilyExpiresAt
			}
			scope := t.Scope
			if requested != "" {
				scope = requested
			}
			return domain.RefreshToken{TokenHash: HashToken(newToken), FamilyID: t.FamilyID, TenantID: t.TenantID,
				PrincipalID: t.PrincipalID, ClientID: t.ClientID, SID: t.SID, Scope: scope, DPoPJKT: t.DPoPJKT,
				IssuedAt: now, ExpiresAt: exp, FamilyExpiresAt: t.FamilyExpiresAt}
		})
	if bindErr != nil {
		return nil, bindErr
	}
	if errors.Is(err, domain.ErrRefreshReused) {
		p.o.Metrics.RefreshReuse()
		corr := p.correlation(r)
		_, _ = p.endLoginSession(ctx, old.TenantID, old.SID, domain.SessionEndRefreshReuse, "system", corr)
		p.emit(ctx, "identity.refresh_token.reuse_detected", old.TenantID, old.PrincipalID, corr, old.FamilyID,
			map[string]any{"client_id": c.ClientID, "sid": old.SID, "family_id": old.FamilyID, "principal_id": old.PrincipalID})
		p.stream(ctx, old.TenantID, "identity.refresh_token.reuse_detected", siem.SeverityHigh,
			"Rotated refresh token reused for principal "+old.PrincipalID+"; family revoked and login session ended")
		return nil, errInvalidGrant("refresh token reused; the session has been ended")
	}
	if err != nil {
		if errors.Is(err, domain.ErrRefreshInvalid) {
			return nil, errInvalidGrant("refresh token invalid or expired")
		}
		p.o.Log.Error("refresh rotation failed", zap.Error(err))
		return nil, errServer("refresh unavailable")
	}
	sess, pr, oe := p.liveSession(ctx, old.TenantID, old.SID)
	if oe != nil {
		return nil, oe
	}
	scope := old.Scope
	if requested != "" {
		scope = requested
	}
	resp, oe := p.issueTokens(ctx, c, pr, sess, scope, "", old.DPoPJKT, newToken)
	return resp, oe
}

// liveSession returns a login session and its principal only if both are
// still good: an ended session or a disabled principal mints nothing.
func (p *Provider) liveSession(ctx context.Context, tenantID, sid string) (*domain.AuthSession, *domain.Principal, *oauthError) {
	sess, err := p.o.Store.GetAuthSession(ctx, tenantID, sid)
	if err != nil {
		return nil, nil, errServer("session lookup unavailable")
	}
	if sess == nil || !sess.Live(p.o.Now()) {
		return nil, nil, errInvalidGrant("the login session has ended")
	}
	pr, err := p.o.Store.FindByID(ctx, sess.PrincipalID, tenantID)
	if err != nil {
		return nil, nil, errServer("principal lookup unavailable")
	}
	if pr == nil || pr.Status != domain.PrincipalStatusActive {
		return nil, nil, errInvalidGrant("the account is not active")
	}
	return sess, pr, nil
}

// issueTokens mints the access token (+ refresh, + ID token) for a session.
// refreshToken is the rotated token when called from a refresh.
func (p *Provider) issueTokens(ctx context.Context, c *Client, pr *domain.Principal, sess *domain.AuthSession,
	scope, nonce, jkt, refreshToken string) (*tokenResponse, *oauthError) {
	at, err := p.o.AccessTokens.IssueGrant(domain.TokenGrant{
		Subject: pr.IdentityProviderSubject, TenantID: pr.TenantID,
		MFADone:  sess.AssuranceLevel != domain.AssuranceSingleFactor,
		AuthTime: sess.AuthTime, AMR: sess.AMR, ClientID: c.ClientID, SID: sess.SID, CnfJKT: jkt,
		Scope: scope, AssuranceLevel: sess.AssuranceLevel,
	})
	if err != nil {
		p.o.Log.Error("access token not minted", zap.Error(err))
		return nil, errServer("token issuance unavailable")
	}
	resp := &tokenResponse{AccessToken: at, TokenType: "Bearer", ExpiresIn: p.o.AccessTokens.TTLSeconds(), Scope: scope}
	if jkt != "" {
		resp.TokenType = "DPoP"
	}
	if refreshToken == "" && c.Allows(GrantRefreshToken) && hasScope(scope, "offline_access") {
		now := p.o.Now()
		famExp := now.Add(p.ap.RefreshTTL)
		if famExp.After(sess.ExpiresAt) {
			famExp = sess.ExpiresAt
		}
		exp := now.Add(p.ap.RefreshIdleTTL)
		if exp.After(famExp) {
			exp = famExp
		}
		refreshToken = RandomToken(32)
		if err := p.o.Store.InsertRefreshToken(ctx, domain.RefreshToken{TokenHash: HashToken(refreshToken),
			FamilyID: ulid.Make().String(), TenantID: pr.TenantID, PrincipalID: pr.PrincipalID, ClientID: c.ClientID,
			SID: sess.SID, Scope: scope, DPoPJKT: jkt, IssuedAt: now, ExpiresAt: exp, FamilyExpiresAt: famExp}); err != nil {
			p.o.Log.Error("refresh token not stored", zap.Error(err))
			return nil, errServer("token issuance unavailable")
		}
	}
	resp.RefreshToken = refreshToken
	if hasScope(scope, "openid") {
		idt, err := p.mintIDToken(c, pr, sess, nonce, scope, at)
		if err != nil {
			return nil, errServer("id token unavailable")
		}
		resp.IDToken = idt
	}
	return resp, nil
}

var verifierRe = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`)

func validVerifier(v string) bool { return verifierRe.MatchString(v) }

var serviceAudience = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}-svc$`)

// audienceRegistered reports whether aud may be minted at all.
func (p *Provider) audienceRegistered(aud string) bool {
	if len(p.ap.ExchangeAudiences) == 0 {
		return serviceAudience.MatchString(aud) || aud == "zoiko-edge"
	}
	return containsString(p.ap.ExchangeAudiences, aud)
}

// grantExchange is RFC 8693 token exchange (AU-12): an envelope for a
// narrower audience, keeping the session (revoking one revokes both) and
// naming the exchanging workload in act. Only a workload — an mTLS peer in
// OIDC_EXCHANGE_ACTORS, or a workload client allowed the grant — may ask.
func (p *Provider) grantExchange(r *http.Request, c *Client) (*tokenResponse, *oauthError) {
	ctx := r.Context()
	if c.ClientType != ClientWorkload {
		return nil, errUnauthorizedClient("token exchange is for workload clients")
	}
	peer := p.o.PeerName(r)
	actor := c.ClientID
	if peer != "" {
		actor = peer
	}
	if p.ap.ExchangeRequireActor && (peer == "" || !containsString(p.ap.ExchangeActors, peer)) {
		return nil, errUnauthorizedClient("token exchange requires a registered workload over mTLS")
	}
	if t := r.PostFormValue("subject_token_type"); t != envelopeTokenType && t != "urn:ietf:params:oauth:token-type:jwt" {
		return nil, errInvalidRequest("subject_token_type must be " + envelopeTokenType)
	}
	aud := strings.TrimSpace(firstOf(r.PostFormValue("audience"), r.PostFormValue("resource")))
	if aud == "" || strings.Contains(aud, " ") {
		return nil, errInvalidTarget("exactly one audience is required")
	}
	if !p.audienceRegistered(aud) || !c.AudienceAllowed(aud) {
		return nil, errInvalidTarget("audience " + aud + " is not allowed for this client")
	}
	subject := r.PostFormValue("subject_token")
	env, err := auth.VerifyAnyAudience(p.o.Ring, p.o.Cfg.JWTIssuer, subject)
	if err != nil {
		return nil, errInvalidGrant("subject_token is not a valid identity envelope")
	}
	if st := p.sessionStatus(ctx, env); !st.Active {
		return nil, errInvalidGrant("the subject's session has ended")
	}
	now := p.o.Now()
	exp := now.Add(p.ap.ExchangeTTL).Unix()
	if env.EXP < exp {
		exp = env.EXP
	}
	out := *env
	out.JTI = ulid.Make().String()
	out.AUD = aud
	out.IAT = now.Unix()
	out.EXP = exp
	out.Act = &domain.Actor{Subject: actor}
	out.Cnf = nil
	// Bound to the exchanging workload's certificate when it called over
	// mTLS (RFC 8705): the narrowed envelope is useful only to that workload.
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		out.Cnf = &domain.Confirmation{X5TS256: idenv.CertThumbprint(r.TLS.PeerCertificates[0])}
	}
	tok, err := p.o.Ring.Sign(&out, "JWT")
	if err != nil {
		return nil, errServer("token issuance unavailable")
	}
	p.emit(ctx, "identity.token.exchanged", env.TenantID, env.Principal.PrincipalID, p.correlation(r), out.JTI, map[string]any{
		"actor": actor, "audience": aud, "session_context_id": env.SessionTrustPosture.SessionContextID, "subject_jti": env.JTI,
	})
	return &tokenResponse{AccessToken: tok, TokenType: "N_A", ExpiresIn: int(exp - now.Unix()), IssuedTokenType: envelopeTokenType}, nil
}

// grantClientCredentials issues a workload's own envelope (IAM §16): never a
// person's, tenant-scoped to the client's registered tenant, audience-bound,
// and certificate-bound when the workload called over mTLS.
func (p *Provider) grantClientCredentials(r *http.Request, c *Client) (*tokenResponse, *oauthError) {
	if c.ClientType != ClientWorkload {
		return nil, errUnauthorizedClient("client credentials are reserved for workload clients")
	}
	aud := strings.TrimSpace(firstOf(r.PostFormValue("audience"), r.PostFormValue("resource")))
	if aud == "" || !p.audienceRegistered(aud) || !c.AudienceAllowed(aud) {
		return nil, errInvalidTarget("a registered audience this client may reach is required")
	}
	tenant := c.TenantID
	if tenant == "" {
		tenant = "platform"
	}
	now := p.o.Now()
	ttl := p.o.Cfg.EnvelopeJWTTTLSeconds
	jti := ulid.Make().String()
	env := domain.IdentityContextEnvelope{
		JTI: jti, ISS: p.o.Cfg.JWTIssuer, AUD: aud, IAT: now.Unix(), EXP: now.Add(time.Duration(ttl) * time.Second).Unix(),
		Principal: domain.PrincipalClaims{PrincipalID: "workload:" + c.ClientID, TenantID: tenant,
			PrincipalType: domain.PrincipalTypeServiceAccount, DisplayName: c.ClientID},
		TenantID: tenant,
		SessionTrustPosture: domain.SessionTrustClaims{Posture: domain.TrustPostureStandard,
			SessionContextID: workloadSessionPrefix + c.ClientID + ":" + jti, AssuranceLevel: domain.AssuranceSingleFactor},
		AuthTime: now.Unix(), AuthorizedParty: c.ClientID, SourceChannel: "system",
		SchemaVersion: "1.0", CorrelationID: p.correlation(r),
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		env.Cnf = &domain.Confirmation{X5TS256: idenv.CertThumbprint(r.TLS.PeerCertificates[0])}
	}
	tok, err := p.o.Ring.Sign(&env, "JWT")
	if err != nil {
		return nil, errServer("token issuance unavailable")
	}
	return &tokenResponse{AccessToken: tok, TokenType: "Bearer", ExpiresIn: ttl, IssuedTokenType: envelopeTokenType}, nil
}

// workloadSessionPrefix marks a workload envelope's session id: its status
// is its client's registration, not a session row.
const workloadSessionPrefix = "wl:"
