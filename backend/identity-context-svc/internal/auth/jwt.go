// Package auth provides JWT verification and envelope signing.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
	return &domain.VerifiedClaims{
		Subject:  claims.Subject,
		TenantID: claims.TenantID,
		MFADone:  claims.MFADone,
	}, nil
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
	cfg *config.Config
}

func NewJWTSigner(cfg *config.Config) *JWTSigner {
	return &JWTSigner{cfg: cfg}
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
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.cfg.JWTSigningSecret))
}
