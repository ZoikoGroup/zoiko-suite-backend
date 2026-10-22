package idenv

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Mode is how the gate treats a request that carries no verifiable caller.
type Mode string

const (
	// ModeRequire refuses it (401). A presented envelope that fails is refused
	// in every mode.
	ModeRequire Mode = "require"
	// ModeWarn admits it as asserted, logged and marked
	// X-Identity-Assertion: missing. A migration state while callers start
	// forwarding the envelope, never a resting one.
	ModeWarn Mode = "warn"
	// ModeOff skips the gate (local development only).
	ModeOff Mode = "off"
)

// ParseMode reads a configured mode. "enforce" and "strict" mean require;
// anything unrecognised requires, so a typo cannot disable the control.
func ParseMode(s string) Mode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "disabled":
		return ModeOff
	case "warn", "observe":
		return ModeWarn
	default:
		return ModeRequire
	}
}

// Headers the gate owns. A client-supplied value is replaced or removed.
const (
	HeaderPrincipalID      = "X-Principal-Id"
	HeaderTenantID         = "X-Tenant-Id"
	HeaderSessionContextID = "X-Session-Context-Id"
	HeaderAuthnAge         = "X-Authn-Age-Seconds"
	HeaderSupportContextID = "X-Support-Context-Id"
	HeaderIdentityBasis    = "X-Identity-Basis"
	HeaderWorkloadID       = "X-Workload-Id"
)

// GateOptions configures NewGate.
type GateOptions struct {
	Service  string
	Mode     Mode
	Verifier *Verifier
	// Revocation checks the envelope's session at identity-context (AU-01).
	// nil skips it (ModeOff deployments only; NewGateFromEnv always sets it).
	Revocation *Revocation
	// DPoP verifies proofs for DPoP-bound envelopes (AU-27).
	DPoP *DPoPVerifier
	// EnforceBinding refuses a sender-constrained envelope without its proof.
	// When false the failure is logged and admitted.
	EnforceBinding bool
	// Peers are the workloads (verified client-certificate names) that may
	// vouch for X-Principal-Id without an envelope, and may forward a
	// sender-constrained envelope they already verified.
	Peers []string
	// TrustAllPeers treats every verified client certificate as a peer.
	TrustAllPeers bool
	// Exempt skips the gate; default: probes, metrics and /.well-known/.
	Exempt func(r *http.Request) bool
	// PeerName returns the verified client-certificate name, "" when the
	// request did not arrive over verified mTLS. Default reads r.TLS.
	PeerName func(r *http.Request) string
	Log      *zap.Logger
	Observer Observer
	Now      func() time.Time
}

// Gate is the relying-party identity check.
type Gate struct {
	o     GateOptions
	peers map[string]bool
}

// NewGate builds a gate.
func NewGate(o GateOptions) (*Gate, error) {
	if o.Mode != ModeOff && o.Verifier == nil {
		return nil, errors.New("idenv: gate needs a verifier unless mode is off")
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	if o.Observer == nil {
		o.Observer = NopObserver{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Exempt == nil {
		o.Exempt = DefaultExempt
	}
	if o.PeerName == nil {
		o.PeerName = VerifiedPeerName
	}
	if o.DPoP == nil {
		o.DPoP = NewDPoPVerifier()
	}
	peers := map[string]bool{}
	for _, p := range o.Peers {
		if p = strings.TrimSpace(p); p != "" {
			peers[p] = true
		}
	}
	return &Gate{o: o, peers: peers}, nil
}

// Mode is the configured mode.
func (g *Gate) Mode() Mode { return g.o.Mode }

// Verifier is the envelope verifier (nil in ModeOff).
func (g *Gate) Verifier() *Verifier { return g.o.Verifier }

// Revocation is the revocation checker (nil when unset).
func (g *Gate) Revocation() *Revocation { return g.o.Revocation }

// DefaultExempt skips probes, metrics and discovery documents.
func DefaultExempt(r *http.Request) bool {
	switch r.URL.Path {
	case "/health", "/healthz", "/readyz", "/livez", "/metrics":
		return true
	}
	return strings.HasPrefix(r.URL.Path, "/.well-known/")
}

// VerifiedPeerName is the CommonName (else first DNS SAN) of a client
// certificate the TLS stack verified; "" otherwise.
func VerifiedPeerName(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.VerifiedChains[0]) == 0 {
		return ""
	}
	leaf := r.TLS.VerifiedChains[0][0]
	if leaf.Subject.CommonName != "" {
		return leaf.Subject.CommonName
	}
	if len(leaf.DNSNames) > 0 {
		return leaf.DNSNames[0]
	}
	return ""
}

func (g *Gate) peerAllowed(name string) bool {
	return name != "" && (g.o.TrustAllPeers || g.peers[name])
}

// Credential extracts the envelope from Authorization: "Bearer <t>" or
// "DPoP <t>" (RFC 9449 §7.1). scheme is lower-cased.
func Credential(r *http.Request) (token, scheme string, ok bool) {
	s, t, found := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !found {
		return "", "", false
	}
	s = strings.ToLower(s)
	t = strings.TrimSpace(t)
	if t == "" || (s != "bearer" && s != "dpop") {
		return "", "", false
	}
	return t, s, true
}

// Authenticate runs every check on r and returns the identity, or the
// problem to answer with. It does not write the response.
func (g *Gate) Authenticate(r *http.Request) (*Identity, *Problem) {
	peer := g.o.PeerName(r)
	token, scheme, hasToken := Credential(r)
	if hasToken {
		return g.authenticateEnvelope(r, token, scheme, peer)
	}
	if g.peerAllowed(peer) {
		g.o.Observer.Decision(OutcomePeer)
		return &Identity{Basis: BasisPeer, Workload: peer,
			PrincipalID: strings.TrimSpace(r.Header.Get(HeaderPrincipalID)),
			TenantID:    strings.TrimSpace(r.Header.Get(HeaderTenantID))}, nil
	}
	if g.o.Mode == ModeRequire {
		g.o.Observer.Decision(OutcomeMissing)
		return nil, &Problem{Status: http.StatusUnauthorized, Code: "identity_assertion_required",
			Title:  "Authentication required",
			Detail: "send the caller's identity envelope as Authorization: Bearer, or call over mTLS as a registered peer workload"}
	}
	g.o.Observer.Decision(OutcomeAsserted)
	return &Identity{Basis: BasisAsserted,
		PrincipalID: strings.TrimSpace(r.Header.Get(HeaderPrincipalID)),
		TenantID:    strings.TrimSpace(r.Header.Get(HeaderTenantID))}, nil
}

func (g *Gate) authenticateEnvelope(r *http.Request, token, scheme, peer string) (*Identity, *Problem) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	claims, err := g.o.Verifier.Verify(ctx, token)
	if err != nil {
		if errors.Is(err, ErrWrongAudience) {
			g.o.Observer.Decision(OutcomeAudience)
			g.o.Log.Warn("identity envelope for another audience refused",
				zap.String("path", r.URL.Path), zap.Strings("accepted", g.o.Verifier.Audiences()))
			return nil, &Problem{Status: http.StatusUnauthorized, Code: "identity_audience_mismatch",
				Title: "Envelope issued for another service", Detail: "the identity envelope was not issued for this service"}
		}
		g.o.Observer.Decision(OutcomeInvalid)
		uc := unverifiedClaims(token)
		g.o.Log.Warn("identity envelope refused", zap.String("path", r.URL.Path),
			zap.String("claimed_principal", uc.Principal.PrincipalID), zap.Error(err))
		return nil, &Problem{Status: http.StatusUnauthorized, Code: "identity_assertion_invalid",
			Title: "Invalid identity envelope", Detail: "the identity envelope in Authorization did not verify"}
	}

	if g.o.Revocation != nil {
		if err := g.o.Revocation.Check(ctx, token, claims); err != nil {
			if errors.Is(err, ErrRevoked) {
				g.o.Observer.Decision(OutcomeRevoked)
				g.o.Log.Info("revoked identity session refused", zap.String("path", r.URL.Path),
					zap.String("principal_id", claims.PrincipalID()),
					zap.String("session_context_id", claims.SessionContextID()), zap.Error(err))
				return nil, &Problem{Status: http.StatusUnauthorized, Code: "session_revoked",
					Title: "Session ended", Detail: "the session this identity envelope was issued for has ended; sign in again"}
			}
			g.o.Observer.Decision(OutcomeUnavailable)
			g.o.Log.Error("session status unavailable — refusing (fail closed)", zap.String("path", r.URL.Path), zap.Error(err))
			return nil, &Problem{Status: http.StatusServiceUnavailable, Code: "identity_status_unavailable", Retryable: true,
				Title: "Session status unavailable", Detail: "the identity service could not confirm the session is still live",
				Extra: map[string]any{"retry_after_seconds": 1}}
		}
	}

	if p := g.checkBinding(r, token, scheme, peer, claims); p != nil {
		return nil, p
	}

	if hp := strings.TrimSpace(r.Header.Get(HeaderPrincipalID)); hp != "" && hp != claims.PrincipalID() {
		g.o.Observer.Decision(OutcomeMismatch)
		g.o.Log.Warn("X-Principal-Id contradicts the verified envelope — refused", zap.String("path", r.URL.Path),
			zap.String("envelope_principal", claims.PrincipalID()), zap.String("header_principal", hp))
		return nil, &Problem{Status: http.StatusForbidden, Code: "identity_mismatch", Title: "Identity mismatch",
			Detail: "X-Principal-Id does not name the principal the identity envelope was issued to"}
	}
	if ht := strings.TrimSpace(r.Header.Get(HeaderTenantID)); ht != "" && ht != claims.TenantID {
		g.o.Observer.Decision(OutcomeMismatch)
		g.o.Log.Warn("X-Tenant-Id contradicts the verified envelope — refused", zap.String("path", r.URL.Path),
			zap.String("envelope_tenant", claims.TenantID), zap.String("header_tenant", ht))
		return nil, &Problem{Status: http.StatusForbidden, Code: "tenant_scope_mismatch", Title: "Tenant mismatch",
			Detail: "X-Tenant-Id does not name the tenant the identity envelope was issued for"}
	}

	g.o.Observer.Decision(OutcomeEnvelope)
	return &Identity{Basis: BasisEnvelope, PrincipalID: claims.PrincipalID(), TenantID: claims.TenantID,
		Claims: claims, Token: token, Workload: peer}, nil
}

// checkBinding enforces cnf (AU-27). A verified peer workload forwarding an
// envelope it received is accepted: the proof was for the hop before it, and
// the workload is itself authenticated by its certificate.
func (g *Gate) checkBinding(r *http.Request, token, scheme, peer string, c *Claims) *Problem {
	if scheme == "dpop" && (c.Cnf == nil || c.Cnf.JKT == "") {
		g.o.Observer.Decision(OutcomeUnbound)
		return &Problem{Status: http.StatusUnauthorized, Code: "invalid_token",
			Title: "Not a DPoP-bound token", Detail: "the DPoP scheme was used with an envelope that is not DPoP-bound"}
	}
	if !c.Cnf.Bound() {
		return nil
	}
	if g.peerAllowed(peer) {
		return nil
	}
	var err error
	switch {
	case c.Cnf.JKT != "":
		var proof *DPoPProof
		proof, err = g.o.DPoP.Verify(r.Header.Get(HeaderDPoP), RequestMethod(r), RequestHTU(r), token)
		if err == nil && proof.JKT != c.Cnf.JKT {
			err = errors.New("dpop: proof key does not match the envelope's cnf.jkt")
		}
	case c.Cnf.X5TS256 != "":
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			err = errors.New("certificate-bound envelope presented without a client certificate")
		} else if CertThumbprint(r.TLS.PeerCertificates[0]) != c.Cnf.X5TS256 {
			err = errors.New("client certificate does not match the envelope's cnf x5t#S256")
		}
	}
	if err == nil {
		return nil
	}
	g.o.Log.Warn("sender-constrained envelope refused", zap.String("path", r.URL.Path),
		zap.String("principal_id", c.PrincipalID()), zap.Bool("enforced", g.o.EnforceBinding), zap.Error(err))
	if !g.o.EnforceBinding {
		return nil
	}
	g.o.Observer.Decision(OutcomeUnbound)
	return &Problem{Status: http.StatusUnauthorized, Code: "invalid_dpop_proof",
		Title: "Sender constraint not satisfied", Detail: err.Error(),
		Extra: map[string]any{"www_authenticate": `DPoP error="invalid_dpop_proof"`}}
}

// Middleware applies the gate.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.o.Mode == ModeOff || g.o.Exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		r.Header.Del(HeaderIdentityBasis)
		id, p := g.Authenticate(r)
		if p != nil {
			if p.Status == http.StatusUnauthorized {
				if v, ok := p.Extra["www_authenticate"].(string); ok {
					w.Header().Set("WWW-Authenticate", v)
				} else {
					w.Header().Set("WWW-Authenticate", `Bearer realm="`+g.o.Service+`", error="invalid_token"`)
				}
			}
			if p.Status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "1")
			}
			WriteProblem(w, r, *p)
			return
		}
		g.stamp(r, id)
		if id.Basis == BasisAsserted {
			g.o.Log.Warn("identity not verified — admitted (warn mode)", zap.String("path", r.URL.Path),
				zap.String("claimed_principal", id.PrincipalID), zap.String("remote_addr", r.RemoteAddr))
			w.Header().Set("X-Identity-Assertion", "missing")
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

// stamp makes the verified identity the only source of the identity headers
// every handler already reads.
func (g *Gate) stamp(r *http.Request, id *Identity) {
	r.Header.Set(HeaderIdentityBasis, string(id.Basis))
	switch id.Basis {
	case BasisEnvelope:
		c := id.Claims
		r.Header.Set(HeaderPrincipalID, c.PrincipalID())
		r.Header.Set(HeaderTenantID, c.TenantID)
		r.Header.Set(HeaderSessionContextID, c.SessionContextID())
		if age, ok := id.AuthnAge(g.o.Now()); ok {
			r.Header.Set(HeaderAuthnAge, strconv.FormatInt(int64(age/time.Second), 10))
		} else {
			r.Header.Del(HeaderAuthnAge)
		}
		if c.SupportContextID != "" {
			r.Header.Set(HeaderSupportContextID, c.SupportContextID)
		} else {
			r.Header.Del(HeaderSupportContextID)
		}
		if id.Workload != "" {
			r.Header.Set(HeaderWorkloadID, id.Workload)
		}
	case BasisPeer:
		r.Header.Set(HeaderWorkloadID, id.Workload)
	case BasisAsserted:
		// Nothing about this request was verified: an age or session id the
		// caller wrote must not reach a step-up check as if it had been.
		r.Header.Del(HeaderAuthnAge)
		r.Header.Del(HeaderSessionContextID)
	}
}
