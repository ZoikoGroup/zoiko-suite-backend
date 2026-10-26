package main

import (
	"os"
	"strconv"

	idenv "zoiko.io/identity-envelope"

	"zoiko.io/policy-svc/internal/config"
)

// gateOptions adapts policy-svc's existing identity settings to the shared
// gate, so a deployment configured for the old verifier keeps its meaning:
//
//	POLICY_IDENTITY_VERIFY (enforce | warn | off)  -> IDENTITY_VERIFY
//	IDENTITY_JWKS_URL                              -> unchanged
//	JWKS_CACHE_TTL                                 -> IDENTITY_JWKS_TTL_SECONDS
//	EXPECTED_ISSUER                                -> IDENTITY_ISSUER
//	EXPECTED_AUDIENCE                              -> IDENTITY_EXTRA_AUDIENCES
//
// A variable already set under its shared name wins. Peers: the old verifier
// admitted any client the mTLS listener admitted, and the listener already
// restricts clients to MTLS_ALLOWED_CLIENTS (or to certificates the platform
// CA issued, when that list is empty), so the gate trusts the same set.
func gateOptions(cfg *config.Config) idenv.EnvOptions {
	setDefault := func(key, value string) {
		if value != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
	setDefault("IDENTITY_VERIFY", cfg.IdentityVerify)
	setDefault("IDENTITY_JWKS_URL", cfg.JWKSURL)
	if s := int(cfg.JWKSCacheTTL.Seconds()); s > 0 {
		setDefault("IDENTITY_JWKS_TTL_SECONDS", strconv.Itoa(s))
	}
	setDefault("IDENTITY_ISSUER", cfg.ExpectedIssuer)
	if cfg.ExpectedAudience != idenv.LegacyAudience {
		setDefault("IDENTITY_EXTRA_AUDIENCES", cfg.ExpectedAudience)
	}
	if cfg.MTLSEnabled && len(cfg.MTLSAllowedClients) == 0 {
		setDefault("IDENTITY_TRUST_MTLS_PEERS", "true")
	}
	return idenv.EnvOptions{DefaultPeers: cfg.MTLSAllowedClients}
}
