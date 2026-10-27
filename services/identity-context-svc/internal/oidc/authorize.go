package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/ratelimit"
	"zoiko.io/identity-context-svc/internal/siem"
)

const (
	pendingTTL     = 10 * time.Minute
	csrfCookie     = "zoiko_login_csrf"
	ssoCookie      = "zoiko_sso"
	maxSecondFails = 5
)

// Authorize is the authorization endpoint (RFC 6749 §4.1.1, OIDC Core
// §3.1.2). An unknown client or an unregistered redirect URI is answered
// here, never redirected (RFC 9700 §4.1: no open redirector).
func (p *Provider) Authorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		p.renderProblem(w, http.StatusBadRequest, "The sign-in request is malformed.")
		return
	}
	q := r.Form
	c, ok := p.o.Clients.Get(q.Get("client_id"))
	if !ok || !c.Allows(GrantAuthorizationCode) {
		p.renderProblem(w, http.StatusBadRequest, "The application that sent you here is not registered.")
		return
	}
	redirectURI := q.Get("redirect_uri")
	if redirectURI == "" || !c.RedirectAllowed(redirectURI) {
		p.renderProblem(w, http.StatusBadRequest, "The application's return address is not registered.")
		return
	}
	state := q.Get("state")
	fail := func(code, desc string) { p.redirectError(w, r, redirectURI, state, code, desc) }

	if q.Get("request") != "" {
		fail("request_not_supported", "request objects are not supported")
		return
	}
	if q.Get("request_uri") != "" {
		fail("request_uri_not_supported", "request_uri is not supported")
		return
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only the authorization code flow is supported (RFC 9700 §2.1.2)")
		return
	}
	if rm := q.Get("response_mode"); rm != "" && rm != "query" {
		fail("invalid_request", "only response_mode=query is supported")
		return
	}
	scope := normalizeScope(q.Get("scope"))
	if !hasScope(scope, "openid") {
		fail("invalid_scope", "the openid scope is required")
		return
	}
	if !scopesAllowed(c, scope) {
		fail("invalid_scope", "a requested scope is not allowed for this client")
		return
	}
	challenge, method := q.Get("code_challenge"), q.Get("code_challenge_method")
	if challenge == "" || method != "S256" || len(challenge) < 43 || len(challenge) > 128 {
		// PKCE S256 for EVERY client, confidential ones included (IAM §18,
		// RFC 9700 §2.1.1). plain is refused.
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	prompt := q.Get("prompt")
	if prompt != "" && prompt != "none" && prompt != "login" {
		fail("invalid_request", "prompt must be none or login")
		return
	}
	maxAge := -1
	if v := q.Get("max_age"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail("invalid_request", "max_age must be a non-negative integer")
			return
		}
		maxAge = n
	}
	pend := &Pending{
		ID: RandomToken(24), ClientID: c.ClientID, RedirectURI: redirectURI, Scope: scope, State: state,
		Nonce: q.Get("nonce"), CodeChallenge: challenge, CodeChallengeMethod: method, DPoPJKT: q.Get("dpop_jkt"),
		TenantHint: strings.TrimSpace(firstOf(q.Get("tenant"), q.Get("tenant_id"))),
		LoginHint:  q.Get("login_hint"), Prompt: prompt, MaxAge: maxAge, ACRValues: q.Get("acr_values"),
		CreatedAt: p.o.Now(),
	}

	// Single sign-on: a live login session in this browser for the same
	// tenant issues a code without asking again, unless the client demanded
	// a fresh login or the session is older than max_age.
	if prompt != "login" {
		if sess, pr := p.ssoSession(r, pend.TenantHint); sess != nil &&
			(maxAge < 0 || sessionAge(sess, p.o.Now()) <= time.Duration(maxAge)*time.Second) &&
			acrSatisfied(pend.ACRValues, sess.AssuranceLevel) {
			p.issueCode(w, r, pend, sess, pr)
			return
		}
		if prompt == "none" {
			fail("login_required", "no live sign-in session")
			return
		}
	}

	csrf := RandomToken(24)
	pend.CSRFHash = HashToken(csrf)
	if err := p.o.Pending.Put(r.Context(), pend, pendingTTL); err != nil {
		p.o.Log.Error("pending authorization not stored", zap.Error(err))
		p.renderProblem(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Try again shortly.")
		return
	}
	p.setCookie(w, csrfCookie, csrf, pendingTTL, http.SameSiteStrictMode)
	p.renderLogin(w, r, loginView{Pending: pend, CSRF: csrf, Email: pend.LoginHint, Tenant: pend.TenantHint})
}

// LoginPassword is the hosted login's first step.
func (p *Provider) LoginPassword(w http.ResponseWriter, r *http.Request) {
	pend, csrf, ok := p.loadPending(w, r)
	if !ok {
		return
	}
	tenant := strings.TrimSpace(firstOf(r.PostFormValue("tenant"), pend.TenantHint))
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	view := loginView{Pending: pend, CSRF: csrf, Email: email, Tenant: tenant}
	if tenant == "" || email == "" || password == "" {
		view.Error = "Enter your organisation, email and password."
		p.renderLogin(w, r, view)
		return
	}
	ctx := r.Context()
	ip := p.clientIP(r)
	if d := p.o.Limiter.Attempt(ctx, "login", ip, tenant); !d.Allowed {
		p.throttled(w, r, d, tenant, view)
		return
	}
	corr := p.correlation(r)
	pr, err := p.o.Passwords.VerifyPassword(ctx, tenant, email, password, corr)
	if err != nil {
		if isUnavailable(err) {
			p.o.Metrics.Login("password", "unavailable")
			p.renderProblem(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Try again shortly.")
			return
		}
		p.o.Limiter.Failure(ctx, ip, tenant)
		p.o.Metrics.Login("password", "failed")
		view.Error = "Sign-in failed. Check your details and try again."
		p.renderLogin(w, r, view)
		return
	}
	if pr.PrincipalType != domain.PrincipalTypeHuman {
		p.o.Metrics.Login("password", "not_human")
		view.Error = "This account cannot sign in interactively."
		p.renderLogin(w, r, view)
		return
	}
	p.o.Metrics.Login("password", "ok")
	pend.TenantID, pend.PrincipalID, pend.PasswordOK = pr.TenantID, pr.PrincipalID, true
	pend.AMR = []string{domain.AMRPassword}
	p.continueAfterFirstFactor(w, r, pend, csrf, pr)
}

// continueAfterFirstFactor asks for a second factor when the principal has
// one (or policy requires one), otherwise completes the login.
func (p *Provider) continueAfterFirstFactor(w http.ResponseWriter, r *http.Request, pend *Pending, csrf string, pr *domain.Principal) {
	ctx := r.Context()
	var factors []string
	if p.o.Passkeys != nil {
		if has, err := p.o.Passkeys.HasPasskey(ctx, pr.TenantID, pr.PrincipalID); err == nil && has {
			factors = append(factors, "passkey")
		}
	}
	if p.o.MFA != nil {
		if has, err := p.o.MFA.HasActiveTOTP(ctx, pr.PrincipalID, pr.TenantID); err == nil && has {
			factors = append(factors, "totp")
		}
	}
	upstreamMFA := containsString(pend.AMR, domain.AMRMFA)
	if len(factors) == 0 || upstreamMFA {
		if p.ap.LoginMFA == "required" && !upstreamMFA {
			p.o.Metrics.Login("second_factor", "not_enrolled")
			p.renderLogin(w, r, loginView{Pending: pend, CSRF: csrf, Tenant: pend.TenantID,
				Error: "Your organisation requires a second factor and none is enrolled for this account. Contact your administrator."})
			return
		}
		p.complete(w, r, pend, pr)
		return
	}
	pend.SecondFactors = factors
	if err := p.o.Pending.Put(ctx, pend, pendingTTL); err != nil {
		p.renderProblem(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Try again shortly.")
		return
	}
	p.renderLogin(w, r, loginView{Pending: pend, CSRF: csrf, Step: "second", Factors: factors, Tenant: pend.TenantID})
}

// LoginTOTP is the second step with a TOTP code.
func (p *Provider) LoginTOTP(w http.ResponseWriter, r *http.Request) {
	pend, csrf, ok := p.loadPending(w, r)
	if !ok {
		return
	}
	if !pend.PasswordOK || !containsString(pend.SecondFactors, "totp") {
		p.renderProblem(w, http.StatusBadRequest, "This sign-in step is out of order. Start again from the application.")
		return
	}
	ctx := r.Context()
	view := loginView{Pending: pend, CSRF: csrf, Step: "second", Factors: pend.SecondFactors, Tenant: pend.TenantID}
	ip := p.clientIP(r)
	if d := p.o.Limiter.Attempt(ctx, "login-mfa", ip, pend.TenantID); !d.Allowed {
		p.throttled(w, r, d, pend.TenantID, view)
		return
	}
	pr, err := p.o.Store.FindByID(ctx, pend.PrincipalID, pend.TenantID)
	if err != nil || pr == nil || pr.Status != domain.PrincipalStatusActive {
		p.renderProblem(w, http.StatusUnauthorized, "This account can no longer sign in.")
		return
	}
	if err := p.o.MFA.VerifyLoginTOTP(ctx, pr, r.PostFormValue("code"), p.correlation(r)); err != nil {
		if isUnavailable(err) {
			p.renderProblem(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Try again shortly.")
			return
		}
		p.o.Limiter.Failure(ctx, ip, pend.TenantID)
		p.o.Metrics.Login("totp", "failed")
		pend.FailedSecond++
		if pend.FailedSecond >= maxSecondFails {
			_ = p.o.Pending.Delete(ctx, pend.ID)
			p.renderProblem(w, http.StatusUnauthorized, "Too many incorrect codes. Start again from the application.")
			return
		}
		_ = p.o.Pending.Put(ctx, pend, pendingTTL)
		view.Error = "That code was not accepted. Try the current code."
		p.renderLogin(w, r, view)
		return
	}
	p.o.Metrics.Login("totp", "ok")
	pend.AMR = append(pend.AMR, domain.AMROTP)
	p.complete(w, r, pend, pr)
}

// LoginPasskeyBegin returns WebAuthn assertion options for the second step.
func (p *Provider) LoginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	pend, _, ok := p.loadPendingJSON(w, r)
	if !ok {
		return
	}
	if p.o.Passkeys == nil || !pend.PasswordOK || !containsString(pend.SecondFactors, "passkey") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passkey_not_available"})
		return
	}
	pr, err := p.o.Store.FindByID(r.Context(), pend.PrincipalID, pend.TenantID)
	if err != nil || pr == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "principal_unavailable"})
		return
	}
	opts, sessionKey, err := p.o.Passkeys.BeginLogin(r.Context(), pr)
	if err != nil {
		p.o.Log.Warn("passkey begin failed", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "passkey_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"options": json.RawMessage(opts), "session": sessionKey})
}

// LoginPasskeyFinish verifies the assertion and completes the login.
func (p *Provider) LoginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	pend, _, ok := p.loadPendingJSON(w, r)
	if !ok {
		return
	}
	if p.o.Passkeys == nil || !pend.PasswordOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "passkey_not_available"})
		return
	}
	ctx := r.Context()
	ip := p.clientIP(r)
	if d := p.o.Limiter.Attempt(ctx, "login-mfa", ip, pend.TenantID); !d.Allowed {
		w.Header().Set("Retry-After", d.RetryAfterSeconds())
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "throttled"})
		return
	}
	pr, err := p.o.Store.FindByID(ctx, pend.PrincipalID, pend.TenantID)
	if err != nil || pr == nil || pr.Status != domain.PrincipalStatusActive {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "principal_unavailable"})
		return
	}
	if err := p.o.Passkeys.FinishLogin(ctx, pr, r.Header.Get("X-Passkey-Session"), []byte(r.PostFormValue("assertion"))); err != nil {
		p.o.Limiter.Failure(ctx, ip, pend.TenantID)
		p.o.Metrics.Login("passkey", "failed")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "passkey_rejected"})
		return
	}
	p.o.Metrics.Login("passkey", "ok")
	pend.AMR = append(pend.AMR, domain.AMRHardwareKey)
	target, err := p.completeURL(ctx, w, r, pend, pr)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"redirect": target})
}

// LoginFederate sends the browser to the tenant's enterprise IdP.
func (p *Provider) LoginFederate(w http.ResponseWriter, r *http.Request) {
	if p.o.Federation == nil {
		p.renderProblem(w, http.StatusNotFound, "Enterprise sign-in is not configured.")
		return
	}
	pend, _, ok := p.loadPending(w, r)
	if !ok {
		return
	}
	tenant := firstOf(pend.TenantHint, r.URL.Query().Get("tenant"))
	idps, err := p.o.Federation.ActiveIdPs(r.Context(), tenant)
	if err != nil {
		p.renderProblem(w, http.StatusServiceUnavailable, "Enterprise sign-in is temporarily unavailable.")
		return
	}
	want := chi.URLParam(r, "idpID")
	for i := range idps {
		if idps[i].IdPID == want {
			pend.TenantID = idps[i].TenantID
			_ = p.o.Pending.Put(r.Context(), pend, pendingTTL)
			if err := p.o.Federation.Start(w, r, &idps[i], pend.ID); err != nil {
				p.o.Log.Warn("federation start failed", zap.String("idp_id", want), zap.Error(err))
				p.renderProblem(w, http.StatusBadGateway, "Your organisation's sign-in service could not be reached.")
			}
			return
		}
	}
	p.renderProblem(w, http.StatusNotFound, "That sign-in option is not available for this organisation.")
}

// CompleteExternal resumes a pending authorization after the federation
// callback verified an upstream login for pr (AU-07). upstreamAMR are the
// methods the IdP reported; "mfa" among them satisfies the second factor.
func (p *Provider) CompleteExternal(w http.ResponseWriter, r *http.Request, pendingID string, pr *domain.Principal, idpID string, upstreamMFA bool) {
	pend, err := p.o.Pending.Get(r.Context(), pendingID)
	if err != nil {
		p.renderProblem(w, http.StatusBadRequest, "This sign-in request has expired. Start again from the application.")
		return
	}
	if pend.TenantID != "" && pend.TenantID != pr.TenantID {
		p.renderProblem(w, http.StatusForbidden, "That account belongs to a different organisation.")
		return
	}
	pend.TenantID, pend.PrincipalID, pend.PasswordOK, pend.IdPID = pr.TenantID, pr.PrincipalID, true, idpID
	pend.AMR = []string{domain.AMRFederated}
	if upstreamMFA {
		pend.AMR = append(pend.AMR, domain.AMRMFA)
	}
	csrf := RandomToken(24)
	pend.CSRFHash = HashToken(csrf)
	p.setCookie(w, csrfCookie, csrf, pendingTTL, http.SameSiteLaxMode)
	p.continueAfterFirstFactor(w, r, pend, csrf, pr)
}

// complete ends the login: records the session, sets the SSO cookie, issues
// the code and redirects.
func (p *Provider) complete(w http.ResponseWriter, r *http.Request, pend *Pending, pr *domain.Principal) {
	target, err := p.completeURL(r.Context(), w, r, pend, pr)
	if err != nil {
		p.renderProblem(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Try again shortly.")
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (p *Provider) completeURL(ctx context.Context, w http.ResponseWriter, r *http.Request, pend *Pending, pr *domain.Principal) (string, error) {
	now := p.o.Now()
	sess := domain.AuthSession{
		SID: RandomToken(18), TenantID: pr.TenantID, PrincipalID: pr.PrincipalID, ClientID: pend.ClientID,
		AuthTime: now, AMR: pend.AMR, AssuranceLevel: domain.AssuranceFor(pend.AMR), IdPID: pend.IdPID,
		CreatedAt: now, ExpiresAt: now.Add(p.ap.LoginSessionMax), ClientIPHash: ratelimit.HashIP(p.clientIP(r)),
	}
	if err := p.o.Store.CreateAuthSession(ctx, sess); err != nil {
		p.o.Log.Error("login session not recorded", zap.Error(err))
		return "", err
	}
	corr := p.correlation(r)
	p.emit(ctx, "identity.login_session.started", pr.TenantID, pr.PrincipalID, corr, sess.SID, map[string]any{
		"sid": sess.SID, "principal_id": pr.PrincipalID, "tenant_id": pr.TenantID, "client_id": pend.ClientID,
		"amr": pend.AMR, "assurance_level": sess.AssuranceLevel, "idp_id": pend.IdPID,
	})
	p.setCookie(w, ssoCookie, pr.TenantID+"."+sess.SID, p.ap.LoginSessionMax, http.SameSiteLaxMode)
	clearCookie(w, csrfCookie, p.secureCookies())
	return p.codeRedirect(ctx, pend, &sess, pr)
}

// issueCode redirects with a fresh code for an existing session (SSO).
func (p *Provider) issueCode(w http.ResponseWriter, r *http.Request, pend *Pending, sess *domain.AuthSession, pr *domain.Principal) {
	target, err := p.codeRedirect(r.Context(), pend, sess, pr)
	if err != nil {
		p.renderProblem(w, http.StatusServiceUnavailable, "Sign-in is temporarily unavailable. Try again shortly.")
		return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (p *Provider) codeRedirect(ctx context.Context, pend *Pending, sess *domain.AuthSession, pr *domain.Principal) (string, error) {
	code := RandomToken(32)
	if err := p.o.Store.InsertAuthCode(ctx, domain.AuthorizationCode{
		CodeHash: HashToken(code), TenantID: pr.TenantID, ClientID: pend.ClientID, RedirectURI: pend.RedirectURI,
		SID: sess.SID, PrincipalID: pr.PrincipalID, Scope: pend.Scope, Nonce: pend.Nonce,
		CodeChallenge: pend.CodeChallenge, CodeChallengeMethod: pend.CodeChallengeMethod, DPoPJKT: pend.DPoPJKT,
		ExpiresAt: p.o.Now().Add(p.ap.CodeTTL),
	}); err != nil {
		p.o.Log.Error("authorization code not stored", zap.Error(err))
		return "", err
	}
	if pend.CSRFHash != "" {
		_ = p.o.Pending.Delete(ctx, pend.ID)
	}
	v := url.Values{"code": {code}, "iss": {p.issuer}}
	if pend.State != "" {
		v.Set("state", pend.State)
	}
	return appendQuery(pend.RedirectURI, v), nil
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (p *Provider) loadPending(w http.ResponseWriter, r *http.Request) (*Pending, string, bool) {
	_ = r.ParseForm()
	id := r.Form.Get("pending")
	csrf := r.Form.Get("csrf")
	pend, err := p.o.Pending.Get(r.Context(), id)
	if err != nil {
		p.renderProblem(w, http.StatusBadRequest, "This sign-in request has expired. Start again from the application.")
		return nil, "", false
	}
	cookie, _ := r.Cookie(csrfCookie)
	if csrf == "" || cookie == nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(csrf)) != 1 ||
		subtle.ConstantTimeCompare([]byte(HashToken(csrf)), []byte(pend.CSRFHash)) != 1 {
		p.renderProblem(w, http.StatusForbidden, "This sign-in form could not be verified. Start again from the application.")
		return nil, "", false
	}
	return pend, csrf, true
}

func (p *Provider) loadPendingJSON(w http.ResponseWriter, r *http.Request) (*Pending, string, bool) {
	_ = r.ParseForm()
	pend, err := p.o.Pending.Get(r.Context(), r.Form.Get("pending"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expired"})
		return nil, "", false
	}
	csrf := r.Form.Get("csrf")
	cookie, _ := r.Cookie(csrfCookie)
	if csrf == "" || cookie == nil || cookie.Value != csrf || HashToken(csrf) != pend.CSRFHash {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf"})
		return nil, "", false
	}
	return pend, csrf, true
}

func (p *Provider) throttled(w http.ResponseWriter, r *http.Request, d ratelimit.Decision, tenant string, view loginView) {
	p.o.Metrics.Throttled(d.Reason)
	corr := p.correlation(r)
	p.emit(r.Context(), "identity.authentication.throttled", tenant, "", corr, ratelimit.HashIP(p.clientIP(r)), map[string]any{
		"reason": d.Reason, "tenant_id": tenant, "client_ip_hash": ratelimit.HashIP(p.clientIP(r)), "retry_after_seconds": d.RetryAfterSeconds(),
	})
	p.stream(r.Context(), tenant, "identity.authentication.throttled", siem.SeverityMedium,
		"Login throttled ("+d.Reason+") from "+ratelimit.HashIP(p.clientIP(r)))
	w.Header().Set("Retry-After", d.RetryAfterSeconds())
	view.Error = "Too many sign-in attempts. Wait a few minutes and try again."
	view.Status = http.StatusTooManyRequests
	p.renderLogin(w, r, view)
}

func (p *Provider) ssoSession(r *http.Request, tenantHint string) (*domain.AuthSession, *domain.Principal) {
	c, err := r.Cookie(ssoCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	tenant, sid, ok := strings.Cut(c.Value, ".")
	if !ok || (tenantHint != "" && tenantHint != tenant) {
		return nil, nil
	}
	sess, err := p.o.Store.GetAuthSession(r.Context(), tenant, sid)
	if err != nil || sess == nil || !sess.Live(p.o.Now()) {
		return nil, nil
	}
	pr, err := p.o.Store.FindByID(r.Context(), sess.PrincipalID, tenant)
	if err != nil || pr == nil || pr.Status != domain.PrincipalStatusActive {
		return nil, nil
	}
	return sess, pr
}

func (p *Provider) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	v := url.Values{"error": {code}, "error_description": {desc}, "iss": {p.issuer}}
	if state != "" {
		v.Set("state", state)
	}
	http.Redirect(w, r, appendQuery(redirectURI, v), http.StatusFound)
}

func (p *Provider) secureCookies() bool { return strings.HasPrefix(p.issuer, "https://") }

func (p *Provider) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration, same http.SameSite) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/oauth2", MaxAge: int(ttl / time.Second),
		HttpOnly: true, Secure: p.secureCookies(), SameSite: same})
}

func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/oauth2", MaxAge: -1, HttpOnly: true, Secure: secure})
}

func appendQuery(base string, v url.Values) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + v.Encode()
}

func normalizeScope(s string) string { return strings.Join(strings.Fields(s), " ") }

func scopesAllowed(c *Client, scope string) bool {
	if len(c.Scopes) == 0 {
		return true
	}
	for _, s := range strings.Fields(scope) {
		if !containsString(c.Scopes, s) {
			return false
		}
	}
	return true
}

func acrSatisfied(requested, assurance string) bool {
	if requested == "" {
		return true
	}
	rank := map[string]int{domain.AssuranceSingleFactor: 0, domain.AssuranceMFA: 1, domain.AssurancePhishingResistant: 2}
	need := 0
	for _, a := range strings.Fields(requested) {
		switch a {
		case acrMFA:
			if need < 1 {
				need = 1
			}
		case acrPhishingResistant:
			need = 2
		}
	}
	return rank[assurance] >= need
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func firstOf(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// isUnavailable reports an infrastructure failure (as opposed to a wrong
// credential) by the error text the context package wraps with.
func isUnavailable(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), "authentication unavailable") {
			return true
		}
	}
	return false
}
