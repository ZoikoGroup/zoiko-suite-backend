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

	// CartaServiceURL is carta-svc — continuous session-risk scoring
	// (Doc 05 §3.11/§5.4's "authentication is not a one-time event").
	// Empty disables the call: risk scoring degrades to "not evaluated",
	// never to a fabricated ALLOW.
	CartaServiceURL string

	// SIEMServiceURL is siem-integration-svc. Empty disables streaming:
	// security-event export is best-effort and tenant-opt-in (a tenant with
	// no configured exporter is normal, not an error), so its absence must
	// never affect whether a request is let through.
	SIEMServiceURL string

	// TenantRegistryURL is tenant-entity-registry-svc — the tenant and legal
	// entity master GOV-01 resolves context against. Empty disables resolution
	// entirely: the gateway then verifies the token and forwards, exactly as it
	// did before, rather than failing every request closed on a dependency the
	// deployment has not configured yet.
	//
	// Once set, resolution is enforced: an unknown or suspended tenant is
	// denied and an unreachable registry blocks material writes. That is the
	// intended end state — see internal/tenantctx.
	TenantRegistryURL string

	// TenantContextTTL bounds how long a resolved context is reused before the
	// registry is consulted again. Short enough that a suspension takes effect
	// promptly, long enough that the registry is not in the hot path of every
	// gated request.
	TenantContextTTL time.Duration

	// TenantContextStaleGrace bounds how far past TenantContextTTL an entry may
	// still answer a NON-MATERIAL READ while the registry is unreachable
	// (ZS-SVC-A-001 §4 GOV-01 degradation contract). Zero disables stale reads.
	// It never applies to writes.
	TenantContextStaleGrace time.Duration
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
	ctxTTL, err := intEnv("TENANT_CONTEXT_TTL_SECONDS", 30)
	if err != nil {
		return nil, err
	}
	ctxGrace, err := intEnv("TENANT_CONTEXT_STALE_GRACE_SECONDS", 120)
	if err != nil {
		return nil, err
	}

	return &Config{
		Port:                    port,
		JWKSURL:                 strEnv("IDENTITY_JWKS_URL", "http://identity-svc:8080/.well-known/jwks.json"),
		JWKSCacheTTL:            time.Duration(ttlSeconds) * time.Second,
		ExpectedIssuer:          strEnv("EXPECTED_ISSUER", "identity-context-svc"),
		ExpectedAudience:        strEnv("EXPECTED_AUDIENCE", "zoiko-internal"),
		CartaServiceURL:         strEnv("CARTA_SERVICE_URL", ""),
		SIEMServiceURL:          strEnv("SIEM_SERVICE_URL", ""),
		TenantRegistryURL:       strEnv("TENANT_REGISTRY_URL", ""),
		TenantContextTTL:        time.Duration(ctxTTL) * time.Second,
		TenantContextStaleGrace: time.Duration(ctxGrace) * time.Second,
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
