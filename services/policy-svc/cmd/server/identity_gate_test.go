package main

import (
	"os"
	"testing"
	"time"

	"zoiko.io/policy-svc/internal/config"
)

func clearGateEnv(t *testing.T) {
	for _, k := range []string{"IDENTITY_VERIFY", "IDENTITY_JWKS_URL", "IDENTITY_JWKS_TTL_SECONDS", "IDENTITY_ISSUER",
		"IDENTITY_EXTRA_AUDIENCES", "IDENTITY_TRUST_MTLS_PEERS"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// A deployment configured for policy-svc's old verifier keeps its meaning on
// the shared gate.
func TestGateOptions_CarryTheLegacySettings(t *testing.T) {
	clearGateEnv(t)
	cfg := &config.Config{IdentityVerify: "warn", JWKSURL: "http://id/jwks", JWKSCacheTTL: 2 * time.Minute,
		ExpectedIssuer: "issuer-x", ExpectedAudience: "policy-audience", MTLSEnabled: true,
		MTLSAllowedClients: []string{"governance-decision-log-svc"}}
	opts := gateOptions(cfg)
	for k, want := range map[string]string{
		"IDENTITY_VERIFY": "warn", "IDENTITY_JWKS_URL": "http://id/jwks", "IDENTITY_JWKS_TTL_SECONDS": "120",
		"IDENTITY_ISSUER": "issuer-x", "IDENTITY_EXTRA_AUDIENCES": "policy-audience",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(opts.DefaultPeers) != 1 || opts.DefaultPeers[0] != "governance-decision-log-svc" {
		t.Errorf("peers = %v", opts.DefaultPeers)
	}
	if os.Getenv("IDENTITY_TRUST_MTLS_PEERS") != "" {
		t.Error("an allow-listed listener must not trust every peer")
	}
}

// The shared names win over the legacy ones; the legacy audience is not
// re-added; an mTLS listener without an allow-list trusts its CA's peers, as
// the old verifier did.
func TestGateOptions_SharedNamesWin(t *testing.T) {
	clearGateEnv(t)
	t.Setenv("IDENTITY_VERIFY", "require")
	cfg := &config.Config{IdentityVerify: "warn", ExpectedAudience: "zoiko-internal", MTLSEnabled: true}
	_ = gateOptions(cfg)
	if os.Getenv("IDENTITY_VERIFY") != "require" {
		t.Errorf("IDENTITY_VERIFY overridden: %q", os.Getenv("IDENTITY_VERIFY"))
	}
	if os.Getenv("IDENTITY_EXTRA_AUDIENCES") != "" {
		t.Error("the legacy audience was added as an extra audience")
	}
	if os.Getenv("IDENTITY_TRUST_MTLS_PEERS") != "true" {
		t.Error("an mTLS listener without an allow-list stopped trusting its peers")
	}
}
