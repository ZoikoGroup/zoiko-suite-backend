package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/keys"
)

// IdPTokenIssuer mints the access token that JWTVerifier.VerifyBearer
// accepts: what the OAuth token endpoint (and the internal
// /v1/authenticate) hands a client, and what /v1/context/resolve exchanges
// for an envelope.
//
// The token carries no roles, no legal entity and no permissions. Those are
// resolved by /v1/context/resolve against live state at the moment of use, so
// a role revoked after login cannot be replayed from a stale token.
type IdPTokenIssuer struct {
	ring       *keys.Keyring
	issuer     string
	audience   string
	ttlSeconds int
	now        func() time.Time
}

// IdPTokenAudience binds an access token to the one endpoint entitled to
// consume it. It is deliberately NOT the envelope's audience: the envelope is
// what other services consume, this token is a step towards one, and the two
// must not be interchangeable at a verifier that checks only a signature.
const IdPTokenAudience = "identity-context-svc/resolve"

// ErrTokenTTLInvalid is returned when the configured token lifetime is not
// positive.
var ErrTokenTTLInvalid = errors.New("IdP token TTL must be a positive number of seconds")

// NewIdPTokenIssuer builds an issuer from config and the signing keyring.
func NewIdPTokenIssuer(cfg *config.Config, ring *keys.Keyring) (*IdPTokenIssuer, error) {
	if cfg.IdPTokenTTLSeconds <= 0 {
		return nil, ErrTokenTTLInvalid
	}
	if ring == nil {
		return nil, errors.New("IdP token issuer needs a signing keyring")
	}
	return &IdPTokenIssuer{
		ring:       ring,
		issuer:     cfg.JWTIssuer,
		audience:   IdPTokenAudience,
		ttlSeconds: cfg.IdPTokenTTLSeconds,
		now:        time.Now,
	}, nil
}

// TTLSeconds is the lifetime of tokens this issuer mints.
func (i *IdPTokenIssuer) TTLSeconds() int { return i.ttlSeconds }

// Issue mints a token asserting that subject authenticated within tenantID.
func (i *IdPTokenIssuer) Issue(subject, tenantID string, mfaDone bool) (string, error) {
	amr := []string{domain.AMRPassword}
	if mfaDone {
		amr = append(amr, domain.AMROTP)
	}
	return i.IssueGrant(domain.TokenGrant{Subject: subject, TenantID: tenantID, MFADone: mfaDone, AMR: amr})
}

// IssueGrant mints a token carrying everything g asserts.
//
// auth_time is the moment of authentication, which is NOT iat once a token is
// re-issued (a refresh or a step-up): a relying party asking "how recently did
// this person prove who they are?" reads auth_time (OIDC Core §2). azp is set
// only from a client this service verified.
func (i *IdPTokenIssuer) IssueGrant(g domain.TokenGrant) (string, error) {
	if g.Subject == "" {
		return "", errors.New("subject is required")
	}
	if g.TenantID == "" {
		return "", errors.New("tenant_id is required")
	}
	now := i.now().UTC()
	authTime := g.AuthTime
	if authTime.IsZero() {
		authTime = now
	}
	assurance := g.AssuranceLevel
	if assurance == "" {
		assurance = domain.AssuranceFor(g.AMR)
	}
	claims := accessClaims{
		TenantID:        g.TenantID,
		MFADone:         g.MFADone,
		AuthorizedParty: g.ClientID,
		ClientID:        g.ClientID,
		AuthTime:        jwt.NewNumericDate(authTime),
		AMR:             g.AMR,
		SID:             g.SID,
		Scope:           g.Scope,
		AssuranceLevel:  assurance,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   g.Subject,
			Issuer:    i.issuer,
			Audience:  jwt.ClaimStrings{i.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(i.ttlSeconds) * time.Second)),
			ID:        ulid.Make().String(),
		},
	}
	if g.CnfJKT != "" {
		claims.Cnf = &domain.Confirmation{JKT: g.CnfJKT}
	}
	return i.ring.Sign(claims, TypeAccessToken)
}
