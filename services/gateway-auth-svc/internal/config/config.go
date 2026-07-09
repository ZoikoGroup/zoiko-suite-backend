// Package config loads gateway-auth-svc configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds this service's runtime settings. There is no database or
// message broker here — this service is a stateless JWT verifier called by
// Traefik's ForwardAuth middleware on every gated request.
type Config struct {
	Port int

	// JWKSURL is identity-context-svc's public key endpoint. Envelope JWTs
	// are signed there with RS256; this service only ever holds the public
	// half, fetched over the network, never a private key.
	JWKSURL      string
	JWKSCacheTTL time.Duration

	// ExpectedIssuer/ExpectedAudience must match identity-context-svc's
	// JWTIssuer / JWTAudienceInternal so a token minted for a different
	// purpose can't be replayed here.
	ExpectedIssuer   string
	ExpectedAudience string
}

func Load() (*Config, error) {
	port, err := intEnv("PORT", 8092)
	if err != nil {
		return nil, err
	}
	ttlSeconds, err := intEnv("JWKS_CACHE_TTL_SECONDS", 300)
	if err != nil {
		return nil, err
	}

	return &Config{
		Port:             port,
		JWKSURL:          strEnv("IDENTITY_JWKS_URL", "http://identity-svc:8080/.well-known/jwks.json"),
		JWKSCacheTTL:     time.Duration(ttlSeconds) * time.Second,
		ExpectedIssuer:   strEnv("EXPECTED_ISSUER", "identity-context-svc"),
		ExpectedAudience: strEnv("EXPECTED_AUDIENCE", "zoiko-internal"),
	}, nil
}

func strEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return n, nil
}
