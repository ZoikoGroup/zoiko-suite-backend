// Package domain defines the canonical types for secret-vault-integration-svc.
package domain

import "time"

// Secret represents a secret stored in the vault.
type Secret struct {
	SecretID       string    `json:"secret_id"`
	TenantID       string    `json:"tenant_id"`
	Name           string    `json:"name"`
	Description    string    `json:"description,omitempty"`
	Version        int       `json:"version"`
	Value          string    `json:"value,omitempty"` // Omitted in list responses
	ContentType    string    `json:"content_type"`    // e.g., "application/json", "text/plain"
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	RotationPolicy string    `json:"rotation_policy,omitempty"` // e.g., "90d", "never"
}

// SecretVersion represents a versioned secret value.
type SecretVersion struct {
	SecretID    string    `json:"secret_id"`
	Version     int       `json:"version"`
	Value       string    `json:"value"`
	ContentType string    `json:"content_type"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// KeyPair represents an asymmetric key pair for JWT signing.
type KeyPair struct {
	KeyID        string    `json:"key_id"`
	TenantID     string    `json:"tenant_id"`
	Algorithm    string    `json:"algorithm"` // e.g., "RS256", "ES256"
	PublicKey    string    `json:"public_key"`  // PEM encoded
	PrivateKey   string    `json:"private_key"` // PEM encoded (only returned on creation)
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	Active       bool      `json:"active"`
}

// JWKS represents a JSON Web Key Set for public key distribution.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK represents a JSON Web Key.
type JWK struct {
	Kty string `json:"kty"` // "RSA", "EC"
	Use string `json:"use"` // "sig"
	Kid string `json:"kid"`
	Alg string `json:"alg"` // "RS256", "ES256"
	N   string `json:"n,omitempty"` // RSA modulus
	E   string `json:"e,omitempty"` // RSA exponent
	X   string `json:"x,omitempty"` // EC X coordinate
	Y   string `json:"y,omitempty"` // EC Y coordinate
	Crv string `json:"crv,omitempty"` // EC curve
}

// Errors
type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrSecretNotFound     = errorString("secret not found")
	ErrKeyNotFound        = errorString("key not found")
	ErrVersionNotFound    = errorString("version not found")
	ErrStoreUnavailable   = errorString("store unavailable")
	ErrUnauthorized       = errorString("unauthorized")
	ErrForbidden          = errorString("forbidden")
	ErrConflict           = errorString("conflict")
	ErrInvalidInput       = errorString("invalid input")
)