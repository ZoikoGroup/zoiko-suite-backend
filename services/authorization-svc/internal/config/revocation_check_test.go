package config

import "testing"

// Register M-03: the mTLS listener checks mtls-management-svc's CRL unless a
// LOCAL stack turns it off.
func TestMTLSRevocationCheck_DefaultAndValidation(t *testing.T) {
	t.Setenv("MTLS_REVOCATION_CHECK", "")
	t.Setenv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls:8140/")
	t.Setenv("MTLS_CRL_URL", "")
	cfg, _ := Load()
	if cfg.MTLSRevocationCheck != "require" || cfg.MTLSCRLURL != "http://mtls:8140/v1/mtls/crl" {
		t.Fatalf("defaults: check=%q url=%q", cfg.MTLSRevocationCheck, cfg.MTLSCRLURL)
	}

	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"require, production", Config{Env: "production", EntityRegistryURL: "http://r", EvaluationMTLS: "permit", MTLSEnabled: true, MTLSRevocationCheck: "require"}, true},
		{"off, local", Config{Env: "local", EvaluationMTLS: "permit", MTLSEnabled: true, MTLSRevocationCheck: "off"}, true},
		{"off, production", Config{Env: "production", EntityRegistryURL: "http://r", EvaluationMTLS: "permit", MTLSEnabled: true, MTLSRevocationCheck: "off"}, false},
		{"off, staging, require mode", Config{Env: "staging", EntityRegistryURL: "http://r", EvaluationMTLS: "require", MTLSEnabled: true, PEPAllowList: []string{"x"}, MTLSRevocationCheck: "off"}, false},
		{"unknown value", Config{Env: "local", EvaluationMTLS: "permit", MTLSRevocationCheck: "best_effort"}, false},
	}
	for _, c := range cases {
		if err := c.cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%s: want ok=%v, got err=%v", c.name, c.ok, err)
		}
	}
}
