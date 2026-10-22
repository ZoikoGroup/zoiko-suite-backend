package idenv

import (
	"context"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// LegacyAudience is the single audience every envelope carried before
// per-service audiences (AU-12). A relying party accepts it only while its
// AcceptLegacyAudience setting is on.
const LegacyAudience = "zoiko-internal"

// Confirmation is the RFC 7800 cnf claim: what the envelope is bound to.
// Exactly one member is set on a sender-constrained envelope.
type Confirmation struct {
	// JKT is the RFC 7638 SHA-256 thumbprint of the DPoP public key (RFC 9449 §6).
	JKT string `json:"jkt,omitempty"`
	// X5TS256 is the SHA-256 thumbprint of the client certificate (RFC 8705 §3.1).
	X5TS256 string `json:"x5t#S256,omitempty"`
}

// Bound reports whether the envelope is sender-constrained.
func (c *Confirmation) Bound() bool { return c != nil && (c.JKT != "" || c.X5TS256 != "") }

// Actor is the RFC 8693 act claim: the workload that exchanged the envelope
// for this audience on the person's behalf.
type Actor struct {
	Subject string `json:"sub"`
}

// Claims is the part of identity-context-svc's envelope a relying party reads.
// The full shape is owned there (domain.IdentityContextEnvelope).
type Claims struct {
	Principal struct {
		PrincipalID   string `json:"principal_id"`
		TenantID      string `json:"tenant_id"`
		PrincipalType string `json:"principal_type"`
		DisplayName   string `json:"display_name"`
	} `json:"principal"`
	TenantID            string `json:"tenant_id"`
	LegalEntityID       string `json:"legal_entity_id"`
	SessionTrustPosture struct {
		Posture           string `json:"posture"`
		MFAVerified       bool   `json:"mfa_verified"`
		AdaptiveRiskScore int    `json:"adaptive_risk_score"`
		SessionContextID  string `json:"session_context_id"`
		// AssuranceLevel is SINGLE_FACTOR, MFA or PHISHING_RESISTANT.
		AssuranceLevel string `json:"assurance_level,omitempty"`
	} `json:"session_trust_posture"`
	SupportContextID string `json:"support_context_id,omitempty"`
	SourceChannel    string `json:"source_channel,omitempty"`
	// AuthTime is when the person last authenticated (Unix seconds).
	AuthTime int64    `json:"auth_time,omitempty"`
	AMR      []string `json:"amr,omitempty"`
	// AuthorizedParty is the verified OAuth client (OIDC azp).
	AuthorizedParty string `json:"azp,omitempty"`
	// SID is the login session the envelope descends from (OIDC sid). Ending
	// it ends every envelope minted under it.
	SID           string        `json:"sid,omitempty"`
	Cnf           *Confirmation `json:"cnf,omitempty"`
	Act           *Actor        `json:"act,omitempty"`
	CorrelationID string        `json:"correlation_id,omitempty"`
	jwt.RegisteredClaims
}

// PrincipalID is the verified principal.
func (c *Claims) PrincipalID() string { return c.Principal.PrincipalID }

// SessionContextID is the session the envelope was issued for.
func (c *Claims) SessionContextID() string { return c.SessionTrustPosture.SessionContextID }

// AuthenticatedAt is AuthTime as a time; zero when the envelope has none.
func (c *Claims) AuthenticatedAt() time.Time {
	if c.AuthTime <= 0 {
		return time.Time{}
	}
	return time.Unix(c.AuthTime, 0).UTC()
}

// Expiry is the envelope's exp; zero when absent (never, after Verify).
func (c *Claims) Expiry() time.Time {
	if c.ExpiresAt == nil {
		return time.Time{}
	}
	return c.ExpiresAt.Time
}

// Basis is how a request's identity was established.
type Basis string

const (
	// BasisEnvelope: a verified envelope named the principal.
	BasisEnvelope Basis = "verified_envelope"
	// BasisPeer: a verified mTLS peer workload vouched for the principal it
	// names in X-Principal-Id (it authenticated the person itself).
	BasisPeer Basis = "mtls_peer"
	// BasisAsserted: nothing was verified (warn mode only).
	BasisAsserted Basis = "asserted"
)

// Identity is the verified caller of a request, on its context.
type Identity struct {
	Basis       Basis
	PrincipalID string
	TenantID    string
	// Claims is set for BasisEnvelope.
	Claims *Claims
	// Token is the raw envelope, for forwarding to the next hop.
	Token string
	// Workload is the verified client-certificate name, when the request came
	// over mTLS (BasisPeer, or an envelope forwarded by a workload).
	Workload string
}

// AuthnAge is how long ago the person authenticated, and false when unknown.
func (id *Identity) AuthnAge(now time.Time) (time.Duration, bool) {
	if id == nil || id.Claims == nil {
		return 0, false
	}
	at := id.Claims.AuthenticatedAt()
	if at.IsZero() {
		return 0, false
	}
	age := now.Sub(at)
	if age < 0 {
		age = 0
	}
	return age, true
}

type ctxKey struct{}

// FromContext returns the request's verified identity.
func FromContext(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(*Identity)
	return id, ok && id != nil
}

// WithIdentity returns ctx carrying id (the gate, and tests).
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}
