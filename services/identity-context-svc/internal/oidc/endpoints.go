package oidc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/auth"
	"zoiko.io/identity-context-svc/internal/domain"
	idenv "zoiko.io/identity-envelope"
)

// ── UserInfo (OIDC Core §5.3) ───────────────────────────────────────────────

// UserInfo answers with the access token's subject.
func (p *Provider) UserInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tok, scheme, ok := idenv.Credential(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	vc, err := p.o.AccessVerifier.VerifyBearer(ctx, tok)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	if vc.CnfJKT != "" {
		if scheme != "dpop" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "error_description": "DPoP-bound token"})
			return
		}
		pf, err := p.dpop.Verify(r.Header.Get(idenv.HeaderDPoP), r.Method, p.issuer+"/oauth2/userinfo", tok)
		if err != nil || pf.JKT != vc.CnfJKT {
			w.Header().Set("WWW-Authenticate", `DPoP error="invalid_dpop_proof"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_dpop_proof"})
			return
		}
	}
	if revoked, err := p.o.Store.AccessTokenRevoked(ctx, vc.TenantID, vc.JTI); err != nil || revoked {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	if vc.SID != "" {
		if sess, err := p.o.Store.GetAuthSession(ctx, vc.TenantID, vc.SID); err != nil || sess == nil || !sess.Live(p.o.Now()) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "error_description": "session ended"})
			return
		}
	}
	pr, err := p.o.Store.FindByIDPSubject(ctx, vc.Subject, vc.TenantID)
	if err != nil || pr == nil || pr.Status != domain.PrincipalStatusActive {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token"})
		return
	}
	out := map[string]any{"sub": pr.PrincipalID, "tenant_id": pr.TenantID}
	if hasScope(vc.Scope, "email") {
		out["email"] = pr.Email
	}
	if hasScope(vc.Scope, "profile") {
		out["name"] = pr.DisplayName
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Revocation (RFC 7009) ───────────────────────────────────────────────────

// Revoke revokes a refresh token (its whole family) or an access token.
func (p *Provider) Revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c, oe := p.authenticateClient(r)
	if oe != nil {
		writeOAuthError(w, oe)
		return
	}
	tok := r.PostFormValue("token")
	if tok == "" {
		writeOAuthError(w, errInvalidRequest("token is required"))
		return
	}
	ctx := r.Context()
	now := p.o.Now()
	if strings.Count(tok, ".") == 2 {
		if vc, err := p.o.AccessVerifier.VerifyBearer(ctx, tok); err == nil && vc.ClientID == c.ClientID {
			if err := p.o.Store.RevokeAccessToken(ctx, vc.TenantID, vc.JTI, vc.ExpiresAt); err != nil {
				writeOAuthError(w, errServer("revocation unavailable"))
				return
			}
		}
	} else if err := p.o.Store.RevokeRefreshToken(ctx, HashToken(tok), c.ClientID, now); err != nil {
		writeOAuthError(w, errServer("revocation unavailable"))
		return
	}
	// RFC 7009 §2.2: 200 whether or not the token was known.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// ── Introspection (RFC 7662) ────────────────────────────────────────────────

// Introspect answers for access tokens and identity envelopes, to
// authenticated confidential or workload clients only.
func (p *Provider) Introspect(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	c, oe := p.authenticateClient(r)
	if oe != nil {
		writeOAuthError(w, oe)
		return
	}
	if c.ClientType == ClientPublic {
		writeOAuthError(w, errUnauthorizedClient("public clients may not introspect"))
		return
	}
	ctx := r.Context()
	tok := r.PostFormValue("token")
	inactive := map[string]any{"active": false}
	if vc, err := p.o.AccessVerifier.VerifyBearer(ctx, tok); err == nil {
		if revoked, err := p.o.Store.AccessTokenRevoked(ctx, vc.TenantID, vc.JTI); err != nil || revoked {
			writeJSON(w, http.StatusOK, inactive)
			return
		}
		if vc.SID != "" {
			if sess, err := p.o.Store.GetAuthSession(ctx, vc.TenantID, vc.SID); err != nil || sess == nil || !sess.Live(p.o.Now()) {
				writeJSON(w, http.StatusOK, inactive)
				return
			}
		}
		out := map[string]any{"active": true, "token_type": "access_token", "sub": vc.Subject, "client_id": vc.ClientID,
			"exp": vc.ExpiresAt.Unix(), "auth_time": vc.AuthTime.Unix(), "amr": vc.AMR, "sid": vc.SID,
			"tenant_id": vc.TenantID, "iss": p.o.Cfg.JWTIssuer, "aud": auth.IdPTokenAudience, "jti": vc.JTI}
		if vc.CnfJKT != "" {
			out["cnf"] = map[string]string{"jkt": vc.CnfJKT}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if env, err := auth.VerifyAnyAudience(p.o.Ring, p.o.Cfg.JWTIssuer, tok); err == nil {
		st := p.sessionStatus(ctx, env)
		if !st.Active {
			writeJSON(w, http.StatusOK, inactive)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"active": true, "token_type": envelopeTokenType,
			"sub": env.Principal.PrincipalID, "tenant_id": env.TenantID, "aud": env.AUD, "exp": env.EXP,
			"session_context_id": env.SessionTrustPosture.SessionContextID, "sid": env.SID})
		return
	}
	writeJSON(w, http.StatusOK, inactive)
}

// ── Logout ──────────────────────────────────────────────────────────────────

// Logout is RP-initiated logout (OIDC RP-Initiated Logout 1.0): it ends the
// login session named by id_token_hint (or this browser's SSO cookie), so
// every envelope resolved under it stops working at every relying party.
func (p *Provider) Logout(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	ctx := r.Context()
	tenant, sid, clientID := "", "", r.Form.Get("client_id")
	if hint := r.Form.Get("id_token_hint"); hint != "" {
		claims := jwt.MapClaims{}
		ks := p.o.Ring.KeySource()
		// An expired hint is still a valid statement of which session to end.
		if _, err := jwt.ParseWithClaims(hint, claims, func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			return ks.PublicKey(ctx, kid)
		}, jwt.WithIssuer(p.issuer), jwt.WithoutClaimsValidation()); err == nil {
			tenant, _ = claims["tenant_id"].(string)
			sid, _ = claims["sid"].(string)
			if aud, _ := claims["aud"].(string); clientID == "" {
				clientID = aud
			}
		}
	}
	if sid == "" {
		if c, err := r.Cookie(ssoCookie); err == nil {
			tenant, sid, _ = strings.Cut(c.Value, ".")
		}
	}
	if sid != "" && tenant != "" {
		if _, err := p.endLoginSession(ctx, tenant, sid, domain.SessionEndLogout, "self", p.correlation(r)); err != nil {
			p.o.Log.Error("logout did not end the login session", zap.Error(err))
		}
	}
	clearCookie(w, ssoCookie, p.secureCookies())
	if target := r.Form.Get("post_logout_redirect_uri"); target != "" {
		if c, ok := p.o.Clients.Get(clientID); ok && c.PostLogoutAllowed(target) {
			if st := r.Form.Get("state"); st != "" {
				target = appendQuery(target, map[string][]string{"state": {st}})
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
	}
	nonce := RandomToken(16)
	p.securityHeaders(w, nonce, "")
	w.WriteHeader(http.StatusOK)
	_ = problemTmpl.Execute(w, map[string]string{"Message": "You have signed out.", "Nonce": nonce})
}

// APILogout ends the caller's own session from a server tier: the envelope
// in Authorization names it. The login session (sid) ends with it, so a
// refresh token cannot mint a replacement.
func (p *Provider) APILogout(w http.ResponseWriter, r *http.Request) {
	tok, _, ok := idenv.Credential(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "identity_assertion_required"})
		return
	}
	env, err := auth.VerifyAnyAudience(p.o.Ring, p.o.Cfg.JWTIssuer, tok)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "identity_assertion_invalid"})
		return
	}
	ctx := r.Context()
	corr := p.correlation(r)
	if env.SID != "" {
		if _, err := p.endLoginSession(ctx, env.TenantID, env.SID, domain.SessionEndLogout, env.Principal.PrincipalID, corr); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logout_unavailable"})
			return
		}
	}
	if sc := env.SessionTrustPosture.SessionContextID; sc != "" && !strings.HasPrefix(sc, workloadSessionPrefix) {
		if err := p.o.Sessions.InvalidateSession(ctx, sc, env.TenantID, domain.InvalidationReasonLogout, env.Principal.PrincipalID, corr); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "logout_unavailable"})
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Session status for relying parties (AU-01) ──────────────────────────────

// SessionStatus answers idenv.Introspector: POST {"token": "<envelope>"}.
// The caller must hold a signed envelope, so a session id alone reveals
// nothing. Any audience this service minted is accepted (exchanged envelopes
// share their session).
func (p *Provider) SessionStatus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || body.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "token is required"})
		return
	}
	env, err := auth.VerifyAnyAudience(p.o.Ring, p.o.Cfg.JWTIssuer, body.Token)
	if err != nil {
		p.o.Metrics.StatusCheck("invalid")
		writeJSON(w, http.StatusOK, idenv.SessionStatus{Active: false, Reason: "INVALID_TOKEN"})
		return
	}
	st := p.sessionStatus(r.Context(), env)
	if st.Reason == "UNAVAILABLE" {
		p.o.Metrics.StatusCheck("unavailable")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "session_status_unavailable"})
		return
	}
	if st.Active {
		p.o.Metrics.StatusCheck("active")
	} else {
		p.o.Metrics.StatusCheck("inactive")
	}
	writeJSON(w, http.StatusOK, st)
}

// sessionStatus decides whether env's session is live: its session context
// exists and is neither invalidated nor expired, its login session (sid) has
// not ended, and its principal is still ACTIVE.
func (p *Provider) sessionStatus(ctx context.Context, env *domain.IdentityContextEnvelope) idenv.SessionStatus {
	scID := env.SessionTrustPosture.SessionContextID
	st := idenv.SessionStatus{SessionContextID: scID, SID: env.SID}
	now := p.o.Now()
	if strings.HasPrefix(scID, workloadSessionPrefix) {
		clientID := strings.SplitN(strings.TrimPrefix(scID, workloadSessionPrefix), ":", 2)[0]
		if c, ok := p.o.Clients.Get(clientID); ok && c.ClientType == ClientWorkload {
			st.Active = true
		} else {
			st.Reason = "WORKLOAD_DISABLED"
		}
		return st
	}
	sc, err := p.o.Store.FindSessionContext(ctx, scID, env.TenantID)
	if err != nil {
		p.o.Log.Error("session status lookup failed", zap.Error(err))
		st.Reason = "UNAVAILABLE"
		return st
	}
	if sc == nil || sc.PrincipalID != env.Principal.PrincipalID {
		st.Reason = "UNKNOWN_SESSION"
		return st
	}
	if sc.InvalidatedAt != nil {
		st.InvalidatedAt = sc.InvalidatedAt.Unix()
		st.Reason = "INVALIDATED"
		if sc.InvalidationReason != nil {
			st.Reason = string(*sc.InvalidationReason)
		}
		return st
	}
	if !now.Before(sc.ExpiresAt.Add(30 * time.Second)) {
		st.Reason = "EXPIRED"
		return st
	}
	sid := firstOf(env.SID, sc.AuthSessionID)
	if sid != "" {
		sess, err := p.o.Store.GetAuthSession(ctx, env.TenantID, sid)
		if err != nil {
			st.Reason = "UNAVAILABLE"
			return st
		}
		if sess == nil || !sess.Live(now) {
			st.Reason = "LOGIN_SESSION_ENDED"
			if sess != nil && sess.EndedAt != nil {
				st.InvalidatedAt = sess.EndedAt.Unix()
				st.Reason = sess.EndReason
			}
			return st
		}
	}
	pr, err := p.o.Store.FindByID(ctx, env.Principal.PrincipalID, env.TenantID)
	if err != nil {
		st.Reason = "UNAVAILABLE"
		return st
	}
	if pr == nil || pr.Status != domain.PrincipalStatusActive {
		st.Reason = "PRINCIPAL_NOT_ACTIVE"
		return st
	}
	st.Active = true
	return st
}
