package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/oklog/ulid/v2"

	"zoiko.io/identity-context-svc/internal/config"
)

// IdPTokenIssuer mints the bearer token that JWTVerifier.VerifyBearer accepts.
//
// Why this exists at all: the resolve path was built against an external
// identity provider that would hand the console a signed OIDC token, and
// VerifyBearer has always known how to check one. No such provider was ever
// wired up, so nothing minted the token and the whole path was unreachable —
// the console filled the gap with hard-coded credentials in the browser tier.
// This issuer is the missing first-party IdP, and it is deliberately the
// smallest thing that closes the gap: it produces a token in exactly the shape
// VerifyBearer already parses, so /v1/context/resolve is unchanged.
//
// It is symmetric (HS256) with the same JWT_SIGNING_SECRET the verifier reads.
// That is sound only because issuer and verifier are the same process holding
// the same secret; the moment a real IdP is introduced, the correct move is to
// point VerifyBearer at that provider's JWKS and delete this type, not to
// share this secret with anyone.
//
// Note the asymmetry with envelope signing: the IDENTITY ENVELOPE is RS256 and
// published via JWKS precisely because other services must verify it without
// holding a key that could mint one. This token never leaves the boundary
// between the console and this service, so it has no such requirement.
type IdPTokenIssuer struct {
	secret     []byte
	issuer     string
	audience   string
	ttlSeconds int
}

// IdPTokenAudience binds a bearer token to the one endpoint entitled to consume
// it.
//
// Without an audience the token asserted only "this subject authenticated" and
// was valid anywhere the signature could be checked. It is deliberately NOT the
// envelope's audience (zoiko-internal): the envelope is what other services
// consume, this token is a single-use step between /v1/authenticate and
// /v1/context/resolve, and the two must not be interchangeable at a verifier
// that checks only a signature.
const IdPTokenAudience = "identity-context-svc/resolve"

// ErrTokenTTLInvalid is returned when the configured token lifetime is not
// positive. Fails at construction rather than minting a token that is already
// expired, or one that never expires.
var ErrTokenTTLInvalid = errors.New("IdP token TTL must be a positive number of seconds")

// NewIdPTokenIssuer builds an issuer from config.
func NewIdPTokenIssuer(cfg *config.Config) (*IdPTokenIssuer, error) {
	if cfg.IdPTokenTTLSeconds <= 0 {
		return nil, ErrTokenTTLInvalid
	}
	// Length is already enforced by config.Load (>= 32 bytes for HS256);
	// re-checked here so the type cannot be constructed weakly from a test.
	if len(cfg.JWTSigningSecret) < 32 {
		return nil, errors.New("JWT signing secret must be at least 32 bytes")
	}
	return &IdPTokenIssuer{
		secret:     []byte(cfg.JWTSigningSecret),
		issuer:     cfg.JWTIssuer,
		audience:   IdPTokenAudience,
		ttlSeconds: cfg.IdPTokenTTLSeconds,
	}, nil
}

// TTLSeconds is the lifetime of tokens this issuer mints, so a handler can
// report expires_in without recomputing it.
func (i *IdPTokenIssuer) TTLSeconds() int { return i.ttlSeconds }

// Issue mints a bearer token asserting that subject authenticated within
// tenantID.
//
// mfaDone is a parameter rather than a constant so the caller decides, but
// there is exactly one honest value to pass today: false. No step-up challenge
// exists anywhere in the estate, so a password is the only factor that can
// have been presented. Passing true would raise the resolved trust posture to
// MFA_VERIFIED on the strength of a password alone, and every downstream
// authorization decision that distinguishes the two would be reading an
// attestation nobody made.
//
// The token carries no roles, no legal entity and no permissions. Those are
// resolved by /v1/context/resolve against live state at the moment of use, so
// a role revoked after login cannot be replayed from a stale token.
func (i *IdPTokenIssuer) Issue(subject, tenantID string, mfaDone bool) (string, error) {
	if subject == "" {
		return "", errors.New("subject is required")
	}
	if tenantID == "" {
		return "", errors.New("tenant_id is required")
	}

	now := time.Now().UTC()
	claims := idpClaims{
		TenantID: tenantID,
		MFADone:  mfaDone,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  subject,
			Issuer:   i.issuer,
			Audience: jwt.ClaimStrings{i.audience},
			// VerifyBearer parses with jwt.WithExpirationRequired(), so exp is
			// not optional — a token without it is rejected, by design.
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Duration(i.ttlSeconds) * time.Second)),
			// A unique ID per token so a replay is identifiable in the audit
			// trail even though this service does not yet maintain a denylist.
			ID: ulid.Make().String(),
		},
	}

	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(i.secret)
}
