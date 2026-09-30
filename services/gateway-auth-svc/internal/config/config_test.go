package config

import (
	"testing"
)

// Enabling mTLS for a peer must change the URL the gateway calls. The 25 Sep
// wiring read the *_MTLS_URL values and never used them, so the mTLS client
// was handed the plain http:// URL and "enabled" sent plaintext.
func TestLoad_MTLSEnabledSwitchesPeerURLs(t *testing.T) {
	t.Setenv("IDENTITY_JWKS_URL", "http://identity-svc:8080/.well-known/jwks.json")
	t.Setenv("TENANT_REGISTRY_URL", "http://tenant-entity-registry-svc:8081")
	t.Setenv("IDENTITY_JWKS_MTLS_ENABLED", "true")
	t.Setenv("IDENTITY_JWKS_MTLS_URL", "https://identity-svc:8449/.well-known/jwks.json")
	t.Setenv("TENANT_REGISTRY_MTLS_ENABLED", "true")
	t.Setenv("TENANT_REGISTRY_MTLS_URL", "https://tenant-entity-registry-svc:8449")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWKSURL != "https://identity-svc:8449/.well-known/jwks.json" {
		t.Errorf("JWKS still called at %q with mTLS enabled", cfg.JWKSURL)
	}
	if cfg.TenantRegistryURL != "https://tenant-entity-registry-svc:8449" {
		t.Errorf("tenant registry still called at %q with mTLS enabled", cfg.TenantRegistryURL)
	}
}

// An mTLS URL that is not https:// would be sent as plaintext. Refuse to boot.
func TestLoad_MTLSEnabledRefusesPlaintextURL(t *testing.T) {
	for _, tc := range []struct{ enable, url string }{
		{"IDENTITY_JWKS_MTLS_ENABLED", "IDENTITY_JWKS_MTLS_URL"},
		{"TENANT_REGISTRY_MTLS_ENABLED", "TENANT_REGISTRY_MTLS_URL"},
	} {
		t.Run(tc.enable, func(t *testing.T) {
			t.Setenv(tc.enable, "true")
			t.Setenv(tc.url, "http://peer:8080")
			if _, err := Load(); err == nil {
				t.Fatalf("%s=true with an http:// URL must fail Load", tc.enable)
			}
		})
	}
}

// Disabled (the default) leaves the plain URLs alone.
func TestLoad_MTLSDisabledKeepsPlainURLs(t *testing.T) {
	t.Setenv("IDENTITY_JWKS_URL", "http://identity-svc:8080/.well-known/jwks.json")
	t.Setenv("TENANT_REGISTRY_URL", "http://tenant-entity-registry-svc:8081")
	t.Setenv("IDENTITY_JWKS_MTLS_ENABLED", "")
	t.Setenv("TENANT_REGISTRY_MTLS_ENABLED", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWKSURL != "http://identity-svc:8080/.well-known/jwks.json" || cfg.TenantRegistryURL != "http://tenant-entity-registry-svc:8081" {
		t.Errorf("mTLS off must not rewrite URLs: jwks=%q registry=%q", cfg.JWKSURL, cfg.TenantRegistryURL)
	}
}
