// Package auth mints and verifies the tokens identity-context-svc issues.
//
// Every token is signed by the keyring (internal/keys, AU-05) and verifiable
// from the published JWKS:
//
//   - the identity ENVELOPE (typ JWT), consumed by every relying party;
//   - the OAuth ACCESS TOKEN (typ at+jwt, RFC 9068), consumed only by
//     POST /v1/context/resolve, which turns it into an envelope;
//   - the OIDC ID TOKEN, consumed by the client that logged the person in.
//
// The access token used to be HS256 under JWT_SIGNING_SECRET, a symmetric
// secret from the environment that could mint as well as verify. It is now
// signed like everything else, so no shared secret exists to leak.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/keys"
	idenv "zoiko.io/identity-envelope"
)

// TypeAccessToken is the JOSE typ of access tokens (RFC 9068 §2.1).
const TypeAccessToken = "at+jwt"

// JWTVerifier verifies the access tokens this service mints.
type JWTVerifier struct {
	ring   *keys.Keyring
	issuer string
	now    func() time.Time
}

// NewJWTVerifier builds a verifier over ring's published keys.
func NewJWTVerifier(cfg *config.Config, ring *keys.Keyring) *JWTVerifier {
	return &JWTVerifier{ring: ring, issuer: cfg.JWTIssuer, now: time.Now}
}

type accessClaims struct {
	TenantID string `json:"tenant_id"`
	MFADone  bool   `json:"mfa_done"`
	// AuthorizedParty is the OIDC azp: the client the token was issued to.
	AuthorizedParty string `json:"azp"`
	ClientID        string `json:"client_id,omitempty"`
	// AuthTime is the OIDC auth_time: when the person authenticated.
	AuthTime *jwt.NumericDate `json:"auth_time,omitempty"`
	// AMR is the authentication methods (RFC 8176).
	AMR            []string            `json:"amr,omitempty"`
	SID            string              `json:"sid,omitempty"`
	Scope          string              `json:"scope,omitempty"`
	AssuranceLevel string              `json:"assurance_level,omitempty"`
	Cnf            *domain.Confirmation `json:"cnf,omitempty"`
	jwt.RegisteredClaims
}

// VerifyBearer parses and validates an access token. Fails closed: any
// unverifiable token returns an error, never partial claims.
func (v *JWTVerifier) VerifyBearer(ctx context.Context, token string) (*domain.VerifiedClaims, error) {
	keySource := v.ring.KeySource()
	parsed, err := jwt.ParseWithClaims(token, &accessClaims{}, func(t *jwt.Token) (any, error) {
		switch t.Method.Alg() {
		case jwt.SigningMethodRS256.Alg(), jwt.SigningMethodES256.Alg():
		default:
			return nil, errors.New("unexpected signing method")
		}
		if typ, _ := t.Header["typ"].(string); !strings.EqualFold(typ, TypeAccessToken) {
			return nil, fmt.Errorf("token typ %q is not an access token", typ)
		}
		kid, _ := t.Header["kid"].(string)
		return keySource.PublicKey(ctx, kid)
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		// A signature says the token was not forged; the audience says it was
		// meant for this door.
		jwt.WithAudience(IdPTokenAudience),
		jwt.WithIssuer(v.issuer),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(*accessClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token claims")
	}
	if claims.Subject == "" {
		return nil, errors.New("token missing sub claim")
	}
	if claims.TenantID == "" {
		return nil, errors.New("token missing tenant_id claim")
	}
	authTime := claims.IssuedAt
	if claims.AuthTime != nil {
		authTime = claims.AuthTime
	}
	if authTime == nil {
		return nil, errors.New("token carries neither auth_time nor iat")
	}
	if authTime.After(v.now().Add(time.Minute)) {
		return nil, errors.New("token auth_time is in the future")
	}
	vc := &domain.VerifiedClaims{
		Subject:        claims.Subject,
		TenantID:       claims.TenantID,
		MFADone:        claims.MFADone,
		AuthTime:       authTime.Time.UTC(),
		AMR:            claims.AMR,
		ClientID:       claims.AuthorizedParty,
		SID:            claims.SID,
		AssuranceLevel: claims.AssuranceLevel,
		JTI:            claims.ID,
		Scope:          claims.Scope,
	}
	if claims.ExpiresAt != nil {
		vc.ExpiresAt = claims.ExpiresAt.Time
	}
	if claims.Cnf != nil {
		vc.CnfJKT = claims.Cnf.JKT
	}
	if vc.AssuranceLevel == "" {
		vc.AssuranceLevel = domain.AssuranceFor(vc.AMR)
	}
	return vc, nil
}

// EnvelopeVerifier verifies an identity envelope this service signed.
//
// The MFA routes, the step-up route, introspection and token exchange
// authenticate their caller by the envelope itself, never by X-Principal-Id.
// It is the exact check every relying party makes (idenv), run against this
// process's own keyring, and it accepts any audience this service minted
// (the session audience and every exchanged one) — the caller is this
// service's own token, wherever it was headed.
type EnvelopeVerifier struct {
	v *idenv.Verifier
}

// NewEnvelopeVerifier builds a verifier for envelopes signed by ring.
// audiences are accepted; empty means any audience.
func NewEnvelopeVerifier(ring *keys.Keyring, issuer string, audiences ...string) *EnvelopeVerifier {
	opts := idenv.VerifierOptions{Keys: ring.KeySource(), Issuer: issuer, AcceptLegacyAudience: true}
	if len(audiences) > 0 {
		opts.Audience = audiences[0]
		opts.ExtraAudiences = audiences[1:]
	}
	v, err := idenv.NewVerifier(opts)
	if err != nil {
		return &EnvelopeVerifier{}
	}
	return &EnvelopeVerifier{v: v}
}

// Verify parses and checks token, returning the envelope it carries.
func (v *EnvelopeVerifier) Verify(token string) (*domain.IdentityContextEnvelope, error) {
	if v == nil || v.v == nil {
		return nil, errors.New("envelope verifier not configured")
	}
	if _, err := v.v.Verify(context.Background(), token); err != nil {
		return nil, err
	}
	return decodeEnvelope(token)
}

// VerifyAnyAudience checks token's signature, issuer and lifetime against
// ring without pinning an audience.
func VerifyAnyAudience(ring *keys.Keyring, issuer, token string) (*domain.IdentityContextEnvelope, error) {
	claims := jwt.MapClaims{}
	keySource := ring.KeySource()
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		switch t.Method.Alg() {
		case jwt.SigningMethodRS256.Alg(), jwt.SigningMethodES256.Alg():
		default:
			return nil, errors.New("unexpected signing method")
		}
		if typ, _ := t.Header["typ"].(string); strings.EqualFold(typ, TypeAccessToken) {
			return nil, errors.New("an access token is not an identity envelope")
		}
		kid, _ := t.Header["kid"].(string)
		return keySource.PublicKey(context.Background(), kid)
	}, jwt.WithExpirationRequired(), jwt.WithIssuer(issuer), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return nil, err
	}
	return decodeEnvelope(token)
}

func decodeEnvelope(token string) (*domain.IdentityContextEnvelope, error) {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, claims); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("re-encode envelope: %w", err)
	}
	var env domain.IdentityContextEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if env.Principal.PrincipalID == "" || env.TenantID == "" {
		return nil, errors.New("envelope missing principal or tenant")
	}
	if env.Principal.TenantID != "" && env.Principal.TenantID != env.TenantID {
		return nil, errors.New("envelope principal and tenant disagree")
	}
	return &env, nil
}

// JWTSigner signs IdentityContextEnvelopes with the keyring's active key.
type JWTSigner struct {
	ring *keys.Keyring
}

// NewJWTSigner builds a signer over ring.
func NewJWTSigner(ring *keys.Keyring) *JWTSigner { return &JWTSigner{ring: ring} }

// Ring is the keyring (JWKS handler, verifiers).
func (s *JWTSigner) Ring() *keys.Keyring { return s.ring }

// Sign produces a signed JWT for the given envelope.
func (s *JWTSigner) Sign(envelope *domain.IdentityContextEnvelope) (string, error) {
	return s.ring.Sign(envelope, "JWT")
}
