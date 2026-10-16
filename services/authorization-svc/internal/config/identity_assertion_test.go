package config

import (
	"testing"
)

// A-01 / A-03 / B-01 configuration, through Load so the defaults are what
// a deployment actually gets.
func loadWith(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	for _, k := range []string{"ENV", "AUTHZ_IDENTITY_ASSERTION", "AUTHZ_IDENTITY_JWKS_URL", "AUTHZ_SESSION_STEP_UP",
		"AUTHZ_SESSION_STEP_UP_MAX_AGE_SECONDS", "AUTHZ_MAX_JIT_SECONDS", "AUTHZ_MAX_BREAK_GLASS_SECONDS",
		"AUTHZ_MAX_SUPPORT_SECONDS", "AUTHZ_ADMIN_DELEGATES"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

func TestIdentityAssertion_Defaults(t *testing.T) {
	cfg, err := loadWith(t, map[string]string{"ENV": "local", "AUTHZ_IDENTITY_JWKS_URL": "http://identity-svc:8080/.well-known/jwks.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IdentityAssertion != "warn" || cfg.SessionStepUp != "enforce" || cfg.SessionStepUpMaxAgeSeconds != 900 {
		t.Fatalf("local defaults: %q %q %d", cfg.IdentityAssertion, cfg.SessionStepUp, cfg.SessionStepUpMaxAgeSeconds)
	}
	if cfg.MaxJITSeconds != 28800 || cfg.MaxBreakGlassSeconds != 7200 || cfg.MaxSupportSeconds != 14400 {
		t.Fatalf("caps: %d %d %d", cfg.MaxJITSeconds, cfg.MaxBreakGlassSeconds, cfg.MaxSupportSeconds)
	}
	t.Setenv("ENV", "production")
	cfg, _ = Load()
	if cfg.IdentityAssertion != "require" {
		t.Fatalf("production default: %q", cfg.IdentityAssertion)
	}
}

func TestIdentityAssertion_Validation(t *testing.T) {
	jwks := "http://identity-svc:8080/.well-known/jwks.json"
	cases := []struct {
		name string
		env  map[string]string
		ok   bool
	}{
		{"local without jwks url", map[string]string{"ENV": "local"}, false},
		{"local off without jwks url", map[string]string{"ENV": "local", "AUTHZ_IDENTITY_ASSERTION": "off"}, true},
		{"off outside local", map[string]string{"ENV": "staging", "AUTHZ_IDENTITY_ASSERTION": "off", "AUTHZ_IDENTITY_JWKS_URL": jwks}, false},
		{"bogus mode", map[string]string{"ENV": "local", "AUTHZ_IDENTITY_ASSERTION": "maybe", "AUTHZ_IDENTITY_JWKS_URL": jwks}, false},
		{"step-up off on a local stack", map[string]string{"ENV": "local", "AUTHZ_IDENTITY_JWKS_URL": jwks, "AUTHZ_SESSION_STEP_UP": "off"}, true},
		{"step-up max age too small", map[string]string{"ENV": "local", "AUTHZ_IDENTITY_JWKS_URL": jwks, "AUTHZ_SESSION_STEP_UP_MAX_AGE_SECONDS": "10"}, false},
		{"jit cap above a day", map[string]string{"ENV": "local", "AUTHZ_IDENTITY_JWKS_URL": jwks, "AUTHZ_MAX_JIT_SECONDS": "90000"}, false},
		{"break-glass cap too small", map[string]string{"ENV": "local", "AUTHZ_IDENTITY_JWKS_URL": jwks, "AUTHZ_MAX_BREAK_GLASS_SECONDS": "5"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadWith(t, c.env)
			if (err == nil) != c.ok {
				t.Fatalf("want ok=%v, got %v", c.ok, err)
			}
		})
	}
	// Step-up can be switched off only on a local stack.
	cfg := &Config{Env: "production", SessionStepUp: "off", EntityRegistryURL: "http://r", EvaluationMTLS: "permit"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("AUTHZ_SESSION_STEP_UP=off accepted in production")
	}
}
