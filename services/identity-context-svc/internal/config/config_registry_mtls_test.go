package config_test

import (
	"strings"
	"testing"

	"zoiko.io/identity-context-svc/internal/config"
)

// tenant-entity-registry-svc runs its identity gate in require mode: a caller
// must present a verified envelope or be a registered mTLS peer. A context
// resolve has no envelope yet (it is what mints one), so this service must call
// the registry as a peer, over mTLS, with its own workload certificate.
// TENANT_REGISTRY_MTLS_URL turns that on; the certificate comes from
// mtls-management-svc, so the URL alone is not enough.

func TestTenantRegistryMTLSURLIsReadFromTheEnvironment(t *testing.T) {
	baseEnv(t)
	t.Setenv("DEPLOY_ENVIRONMENT", "local")
	t.Setenv("TENANT_REGISTRY_MTLS_URL", "https://tenant-entity-registry-svc:8449")
	t.Setenv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140")
	t.Setenv("MTLS_BOOTSTRAP_TOKEN_PATH", "/bootstrap/token")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TenantRegistryMTLSURL != "https://tenant-entity-registry-svc:8449" {
		t.Fatalf("TenantRegistryMTLSURL = %q", cfg.TenantRegistryMTLSURL)
	}
}

func TestTenantRegistryMTLSURLDefaultsToOff(t *testing.T) {
	baseEnv(t)
	t.Setenv("DEPLOY_ENVIRONMENT", "local")
	t.Setenv("TENANT_REGISTRY_MTLS_URL", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TenantRegistryMTLSURL != "" {
		t.Fatalf("TenantRegistryMTLSURL = %q, want empty (plain HTTP to TENANT_REGISTRY_URL)", cfg.TenantRegistryMTLSURL)
	}
}

func TestTenantRegistryMTLSURLNeedsAWayToGetACertificate(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no management URL":  {"MTLS_MANAGEMENT_SERVICE_URL": "", "MTLS_BOOTSTRAP_TOKEN_PATH": "/bootstrap/token"},
		"no bootstrap token": {"MTLS_MANAGEMENT_SERVICE_URL": "http://mtls-management-svc:8140", "MTLS_BOOTSTRAP_TOKEN_PATH": ""},
	} {
		t.Run(name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("DEPLOY_ENVIRONMENT", "local")
			t.Setenv("TENANT_REGISTRY_MTLS_URL", "https://tenant-entity-registry-svc:8449")
			for k, v := range env {
				t.Setenv(k, v)
			}
			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), "TENANT_REGISTRY_MTLS_URL") {
				t.Fatalf("Load error = %v, want one naming TENANT_REGISTRY_MTLS_URL", err)
			}
		})
	}
}
