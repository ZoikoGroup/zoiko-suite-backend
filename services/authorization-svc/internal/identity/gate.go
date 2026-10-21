// Package identity authenticates the caller of authorization-svc's admin,
// session and console routes from the signed identity envelope, instead of
// trusting X-Principal-Id and X-Tenant-Id (Governance audit register A-01;
// Group 1 register R-3).
//
// Why: those routes were reachable on the plaintext port by anything on the
// container network, and console calls reach this service directly rather than
// through the gateway's ForwardAuth (S5-B2), so the headers were whatever the
// caller wrote. A forged X-Principal-Id could create roles, grant access or
// issue a break-glass session in someone else's name.
//
// The envelope identity-context-svc mints already travels in Authorization on
// every console call. This package verifies it exactly as gateway-auth-svc
// does (RS256, the key from identity-context-svc's JWKS, issuer, audience,
// expiry) and makes it the source of the principal and tenant. A header that
// disagrees with it is refused, not obeyed.
//
// Two callers carry no user envelope and are handled explicitly:
//
//   - Policy enforcement points on the evaluation routes. Those are gated by
//     the mTLS workload gate (A-02): the PEP vouches for the principal it asks
//     about. This gate does not apply to them.
//   - A delegate service acting for a person it has already authenticated —
//     access-control-svc provisioning an approved grant, possibly long after
//     the request. AUTHZ_ADMIN_DELEGATES names those services; only a
//     certificate the mTLS listener verified counts, never a header. A delegate
//     that holds the person's envelope should forward it instead, and then it
//     is verified like any other.
//
// The verified facts (principal, tenant, MFA, auth_time) are put on the
// request context as an Assertion. Session issue reads them from there for
// its step-up check (A-03), never from a header.
package identity

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/authorization-svc/internal/workload"
)

// Mode is how a request without a verifiable caller is treated.
type Mode string

const (
	// ModeRequire refuses it 401. The default outside ENV=local.
	ModeRequire Mode = "require"
	// ModeWarn admits it, marked X-Identity-Assertion: missing and logged. A
	// PRESENTED envelope that fails is still refused.
	ModeWarn Mode = "warn"
	// ModeOff skips the gate entirely (ENV=local only; see config).
	ModeOff Mode = "off"
)

// Basis values on an Assertion.
const (
	BasisEnvelope = "envelope"
	BasisDelegate = "delegate"
)

// Assertion is the verified caller of a request.
type Assertion struct {
	PrincipalID      string
	TenantID         string
	SessionContextID string
	// SID is the login session the envelope descends from (OIDC sid);
	// ending it ends every envelope minted under it.
	SID         string
	MFAVerified bool
	// AuthTime is when the person last authenticated; zero when unknown.
	AuthTime time.Time
	AMR      []string
	// Basis is BasisEnvelope (a verified envelope) or BasisDelegate (a
	// verified delegate service vouching for a forwarded principal).
	Basis string
	// Delegate is the delegate service's certificate name, for BasisDelegate.
	Delegate string
}

// AssuranceLevel is MFA or SINGLE_FACTOR for an envelope assertion, "" for a
// delegate (which asserts no authentication of its own).
func (a *Assertion) AssuranceLevel() string {
	if a == nil || a.Basis != BasisEnvelope {
		return ""
	}
	if a.MFAVerified {
		return "MFA"
	}
	return "SINGLE_FACTOR"
}

type ctxKey struct{}

// FromContext returns the request's verified caller, if any.
func FromContext(ctx context.Context) (*Assertion, bool) {
	a, ok := ctx.Value(ctxKey{}).(*Assertion)
	return a, ok && a != nil
}

// WithAssertion returns ctx carrying a. Used by the gate, and by tests that
// exercise a handler without minting an envelope.
func WithAssertion(ctx context.Context, a *Assertion) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// KeySource resolves an envelope signing key by kid.
type KeySource interface {
	PublicKey(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// Gate is the A-01 check. Build it with NewGate.
type Gate struct {
	mode      Mode
	keys      KeySource
	issuer    string
	audience  string
	delegates map[string]bool
	log       *zap.Logger
	now       func() time.Time

	// revocation checks each verified envelope's session at
	// identity-context-svc (AU-01). Nil skips the check (ENV=local only).
	revocation *idenv.Revocation
}

// WithRevocation makes the gate refuse an envelope whose session has ended:
// a logout, an invalidation, a suspended principal or an ended login session.
// Before it, a revoked envelope kept opening the admin routes until it expired
// (Auth audit AU-01: "revocation must not wait for token expiry").
func (g *Gate) WithRevocation(r *idenv.Revocation) *Gate {
	g.revocation = r
	return g
}

// NewGate builds the gate. delegates are service names (client certificate
// CNs) allowed to vouch for a forwarded principal without an envelope.
func NewGate(mode Mode, keys KeySource, issuer, audience string, delegates []string, log *zap.Logger) *Gate {
	d := map[string]bool{}
	for _, s := range delegates {
		if s = strings.TrimSpace(s); s != "" {
			d[s] = true
		}
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &Gate{mode: mode, keys: keys, issuer: issuer, audience: audience, delegates: d, log: log,
		now: time.Now}
}

// Mode returns the configured mode.
func (g *Gate) Mode() Mode { return g.mode }

// Delegates returns the delegate list for the startup log.
func (g *Gate) Delegates() []string {
	out := make([]string, 0, len(g.delegates))
	for s := range g.delegates {
		out = append(out, s)
	}
	return out
}

// Applies reports whether path is gated: everything except the evaluation
// routes (A-02's workload gate) and the probes.
func Applies(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return false
	}
	return !workload.IsEvaluationPath(path)
}

// envelopeClaims is the slice of identity-context-svc's envelope this gate
// reads. The full shape is owned there.
type envelopeClaims struct {
	Principal struct {
		PrincipalID string `json:"principal_id"`
		TenantID    string `json:"tenant_id"`
	} `json:"principal"`
	TenantID            string `json:"tenant_id"`
	SessionTrustPosture struct {
		MFAVerified      bool   `json:"mfa_verified"`
		SessionContextID string `json:"session_context_id"`
	} `json:"session_trust_posture"`
	AuthTime int64    `json:"auth_time"`
	AMR      []string `json:"amr"`
	SID      string   `json:"sid"`
	jwt.RegisteredClaims
}

// ErrInvalidAssertion is any envelope that does not verify.
var ErrInvalidAssertion = errors.New("identity assertion invalid")

// Verify checks token and returns the assertion it carries.
func (g *Gate) Verify(ctx context.Context, token string) (*Assertion, error) {
	claims := &envelopeClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("envelope has no kid")
		}
		return g.keys.PublicKey(ctx, kid)
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(g.issuer),
		jwt.WithAudience(g.audience),
		jwt.WithTimeFunc(g.now),
	)
	if err != nil {
		return nil, errors.Join(ErrInvalidAssertion, err)
	}
	if claims.Principal.PrincipalID == "" || claims.TenantID == "" {
		return nil, errors.Join(ErrInvalidAssertion, errors.New("envelope names no principal or tenant"))
	}
	if claims.Principal.TenantID != "" && claims.Principal.TenantID != claims.TenantID {
		return nil, errors.Join(ErrInvalidAssertion, errors.New("envelope principal and tenant disagree"))
	}
	a := &Assertion{
		PrincipalID:      claims.Principal.PrincipalID,
		TenantID:         claims.TenantID,
		SessionContextID: claims.SessionTrustPosture.SessionContextID,
		SID:              claims.SID,
		MFAVerified:      claims.SessionTrustPosture.MFAVerified,
		AMR:              claims.AMR,
		Basis:            BasisEnvelope,
	}
	if claims.AuthTime > 0 {
		a.AuthTime = time.Unix(claims.AuthTime, 0).UTC()
	}
	return a, nil
}

// Middleware applies the gate.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.mode == ModeOff || !Applies(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		if token, ok := bearer(r.Header.Get("Authorization")); ok {
			a, err := g.Verify(r.Context(), token)
			if err != nil {
				// A presented credential that fails is refused in every mode:
				// "warn" is for callers that have not started sending one, not
				// for credentials that are expired, forged or for someone else.
				g.log.Warn("identity assertion refused", zap.String("path", r.URL.Path), zap.Error(err))
				refuse(w, http.StatusUnauthorized, "identity_assertion_invalid", "AUTHENTICATION_FAILED",
					"the identity envelope in Authorization did not verify")
				return
			}
			if hp := strings.TrimSpace(r.Header.Get("X-Principal-Id")); hp != "" && hp != a.PrincipalID {
				g.log.Warn("X-Principal-Id contradicts the verified envelope — refused",
					zap.String("path", r.URL.Path), zap.String("envelope_principal", a.PrincipalID), zap.String("header_principal", hp))
				refuse(w, http.StatusForbidden, "identity_mismatch", "AUTHORIZATION_DENIED",
					"X-Principal-Id does not name the principal the identity envelope was issued to")
				return
			}
			if ht := strings.TrimSpace(r.Header.Get("X-Tenant-Id")); ht != "" && ht != a.TenantID {
				g.log.Warn("X-Tenant-Id contradicts the verified envelope — refused",
					zap.String("path", r.URL.Path), zap.String("envelope_tenant", a.TenantID), zap.String("header_tenant", ht))
				refuse(w, http.StatusForbidden, "tenant_scope_mismatch", "AUTHORIZATION_DENIED",
					"X-Tenant-Id does not name the tenant the identity envelope was issued for")
				return
			}
			if !g.sessionLive(w, r, token, a) {
				return
			}
			// The verified values become the headers every handler already
			// reads, so no handler can see anything else.
			r.Header.Set("X-Principal-Id", a.PrincipalID)
			r.Header.Set("X-Tenant-Id", a.TenantID)
			next.ServeHTTP(w, r.WithContext(WithAssertion(r.Context(), a)))
			return
		}

		if cn := workload.VerifiedIdentity(r); cn != "" && g.delegates[cn] {
			a := &Assertion{
				PrincipalID: strings.TrimSpace(r.Header.Get("X-Principal-Id")),
				TenantID:    strings.TrimSpace(r.Header.Get("X-Tenant-Id")),
				Basis:       BasisDelegate,
				Delegate:    cn,
			}
			r.Header.Set(workload.HeaderWorkloadID, cn)
			next.ServeHTTP(w, r.WithContext(WithAssertion(r.Context(), a)))
			return
		}

		if g.mode == ModeRequire {
			g.log.Warn("identity assertion missing — refused", zap.String("path", r.URL.Path),
				zap.String("claimed_principal", r.Header.Get("X-Principal-Id")), zap.String("remote_addr", r.RemoteAddr))
			refuse(w, http.StatusUnauthorized, "identity_assertion_required", "AUTHENTICATION_FAILED",
				"send the caller's identity envelope as Authorization: Bearer, or call over mTLS as a registered delegate")
			return
		}
		g.log.Warn("identity assertion missing — admitted (AUTHZ_IDENTITY_ASSERTION=warn)",
			zap.String("path", r.URL.Path), zap.String("claimed_principal", r.Header.Get("X-Principal-Id")))
		w.Header().Set("X-Identity-Assertion", "missing")
		next.ServeHTTP(w, r)
	})
}

// sessionLive runs the revocation check (AU-01) and writes the refusal when
// the session has ended (401 session_revoked) or its status is unknown and
// the check fails closed (503).
func (g *Gate) sessionLive(w http.ResponseWriter, r *http.Request, token string, a *Assertion) bool {
	if g.revocation == nil {
		return true
	}
	c := &idenv.Claims{TenantID: a.TenantID, SID: a.SID}
	c.Principal.PrincipalID = a.PrincipalID
	c.SessionTrustPosture.SessionContextID = a.SessionContextID
	err := g.revocation.Check(r.Context(), token, c)
	switch {
	case err == nil:
		return true
	case errors.Is(err, idenv.ErrRevoked):
		g.log.Warn("identity session revoked — refused", zap.String("path", r.URL.Path),
			zap.String("principal_id", a.PrincipalID), zap.String("session_context_id", a.SessionContextID), zap.Error(err))
		refuse(w, http.StatusUnauthorized, "session_revoked", "AUTHENTICATION_FAILED",
			"the identity envelope's session has ended; sign in again")
	default:
		g.log.Error("identity session status unavailable — failing closed", zap.String("path", r.URL.Path), zap.Error(err))
		refuse(w, http.StatusServiceUnavailable, "session_status_unavailable", "DEPENDENCY_UNAVAILABLE",
			"the identity session could not be confirmed")
	}
	return false
}

func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

func refuse(w http.ResponseWriter, status int, code, class, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="authorization-svc"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_class": class, "message": msg})
}
