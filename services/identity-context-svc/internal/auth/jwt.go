// Package auth provides JWT verification and envelope signing.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"zoiko.io/identity-context-svc/internal/config"
	"zoiko.io/identity-context-svc/internal/domain"
)

// JWTVerifier verifies inbound OIDC bearer tokens from the IdP.
//
// Production: validate against the IdP's JWKS endpoint using RS256.
// TODO: replace HMAC verification with JWKS-backed RSA public key validation
//
//	and integrate with Secret Vault Integration Service for key material.
type JWTVerifier struct {
	cfg *config.Config
}

func NewJWTVerifier(cfg *config.Config) *JWTVerifier {
	return &JWTVerifier{cfg: cfg}
}

type idpClaims struct {
	TenantID string `json:"tenant_id"`
	MFADone  bool   `json:"mfa_done"`
	// AuthorizedParty is the OIDC azp: the client the token was issued to.
	AuthorizedParty string `json:"azp"`
	// AuthTime is the OIDC auth_time: when the person authenticated.
	AuthTime *jwt.NumericDate `json:"auth_time,omitempty"`
	// AMR is the authentication methods (RFC 8176).
	AMR []string `json:"amr,omitempty"`
	jwt.RegisteredClaims
}

// VerifyBearer parses and validates a bearer JWT.
// Returns VerifiedClaims on success; non-nil error on any failure.
// Fails closed — any unverifiable token returns an error, never partial claims.
func (v *JWTVerifier) VerifyBearer(_ context.Context, token string) (*domain.VerifiedClaims, error) {
	parsed, err := jwt.ParseWithClaims(token, &idpClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(v.cfg.JWTSigningSecret), nil
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		// Audience and issuer are both checked. Neither was before, so any
		// token this secret could verify was accepted here — including one
		// minted for a different purpose entirely. A signature says the token
		// was not forged; it does not say the token was meant for this door.
		jwt.WithAudience(IdPTokenAudience),
		jwt.WithIssuer(v.cfg.JWTIssuer),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(*idpClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token claims")
	}
	if claims.Subject == "" {
		return nil, errors.New("token missing sub claim")
	}
	if claims.TenantID == "" {
		return nil, errors.New("token missing tenant_id claim")
	}
	// A token minted before auth_time existed authenticated when it was
	// issued, so iat is its auth_time. jwt.WithIssuedAt above has already
	// refused one issued in the future.
	authTime := claims.IssuedAt
	if claims.AuthTime != nil {
		authTime = claims.AuthTime
	}
	if authTime == nil {
		return nil, errors.New("token carries neither auth_time nor iat")
	}
	if authTime.After(time.Now().Add(time.Minute)) {
		return nil, errors.New("token auth_time is in the future")
	}
	return &domain.VerifiedClaims{
		Subject:  claims.Subject,
		TenantID: claims.TenantID,
		MFADone:  claims.MFADone,
		AuthTime: authTime.Time.UTC(),
		AMR:      claims.AMR,
		ClientID: claims.AuthorizedParty,
	}, nil
}

// EnvelopeVerifier verifies an identity envelope this service signed.
//
// The MFA routes and the step-up route authenticate their caller by the
// envelope itself, never by X-Principal-Id. Enrolling, revoking or using a
// second factor on someone else's behalf must take their session, not a header
// anyone on the network can write (the same gap authorization-svc's A-01
// closes). The envelope is RS256 under the key published at
// /.well-known/jwks.json, so this is the exact check every other relying party
// makes, run with the public half of this process's own key.
type EnvelopeVerifier struct {
	key      *rsa.PublicKey
	issuer   string
	audience string
}

// NewEnvelopeVerifier builds a verifier for envelopes signed by key.
func NewEnvelopeVerifier(key *rsa.PublicKey, issuer, audience string) *EnvelopeVerifier {
	return &EnvelopeVerifier{key: key, issuer: issuer, audience: audience}
}

// Verify parses and checks token, returning the envelope it carries. Any
// failure (signature, algorithm, issuer, audience, expiry, missing principal or
// tenant) is an error, never a partial envelope.
func (v *EnvelopeVerifier) Verify(token string) (*domain.IdentityContextEnvelope, error) {
	if v == nil || v.key == nil {
		return nil, errors.New("envelope verifier not configured")
	}
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return v.key, nil
	},
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
	)
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid envelope claims")
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

// JWTSigner signs IdentityContextEnvelopes as short-lived JWTs (Q2 resolution).
//
// Current implementation: HS256 with a shared secret for development.
// TODO: migrate to RS256 with a KMS-backed private key via Secret Vault
//
//	Integration Service before Phase 1 production cutover.
//	The public key must be published to a JWKS endpoint so downstream
//	services can verify envelopes independently.
type JWTSigner struct {
	cfg        *config.Config
	privateKey *rsa.PrivateKey
}

func NewJWTSigner(cfg *config.Config) (*JWTSigner, error) {
	keyBytes, err := os.ReadFile(cfg.JWTSigningPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	privateKey, err := jwt.ParseRSAPrivateKeyFromPEM(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return &JWTSigner{cfg: cfg, privateKey: privateKey}, nil
}

// Sign produces a signed JWT for the given envelope.
// We marshal the envelope to jwt.MapClaims so that:
//   - All envelope fields are included in the token exactly as serialised.
//   - We avoid the jwt/v5 Claims interface complexity (GetAudience etc.).
//   - No struct-embedding required, eliminating the duplicate-JSON-tag problem.
func (s *JWTSigner) Sign(envelope *domain.IdentityContextEnvelope) (string, error) {
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("marshal envelope: %w", err)
	}
	var claims jwt.MapClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", fmt.Errorf("unmarshal to MapClaims: %w", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.cfg.JWTKeyID
	return token.SignedString(s.privateKey)
}
