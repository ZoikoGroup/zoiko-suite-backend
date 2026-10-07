package config_test

import (
	"testing"

	"go.uber.org/zap"

	"zoiko.io/identity-context-svc/internal/authz"
	"zoiko.io/identity-context-svc/internal/config"
)

// AUTHZ_ENV was a second environment variable naming the same fact as
// DEPLOY_ENVIRONMENT, defaulted to "development", and set by nothing in the
// estate — not compose, not the manifests, not the runbook.
//
// The consequence was not a style problem. authz.NewClient's guard refuses a
// placeholder authorization-svc in production, and it was reading a variable
// that was always "development", so the guard could not fire in any deployment
// that has ever existed. A production service with AUTHZ_SERVICE_URL unset got
// the permit-all stub and a warning log.
//
// These tests hold the two halves together: the tier is derived, and it cannot
// be argued down.

func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SIGNING_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("AUTHZ_ENV", "")
	t.Setenv("AUTHZ_SERVICE_URL", "")
	t.Setenv("SOD_SERVICE_URL", "http://sod-svc:8090")
}

func TestAuthzEnvDefaultsToTheDeploymentEnvironment(t *testing.T) {
	for _, env := range []string{"local", "development", "staging", "production"} {
		t.Run(env, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("DEPLOY_ENVIRONMENT", env)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AuthzEnv != env {
				t.Fatalf("AuthzEnv = %q, want %q — the authorization guard reads this", cfg.AuthzEnv, env)
			}
		})
	}
}

func TestAuthzEnvCannotDowngradeAProtectedTier(t *testing.T) {
	for _, deployEnv := range []string{"production", "staging"} {
		for _, authzEnv := range []string{"development", "local"} {
			baseEnv(t)
			t.Setenv("DEPLOY_ENVIRONMENT", deployEnv)
			t.Setenv("AUTHZ_ENV", authzEnv)

			if _, err := config.Load(); err == nil {
				t.Fatalf("AUTHZ_ENV=%q in DEPLOY_ENVIRONMENT=%q must be refused: it disables the authorization guard",
					authzEnv, deployEnv)
			}
		}
	}
}

// TestProductionDefaultsCannotProduceAPermitAllClient is the regression this
// whole derivation exists for, asserted end to end rather than in two halves:
// take the configuration a production deployment gets when nobody sets
// anything, and build the authorization client from it.
//
// Before the fix this returned a *PermitAllClient and no error.
func TestProductionDefaultsCannotProduceAPermitAllClient(t *testing.T) {
	baseEnv(t)
	t.Setenv("DEPLOY_ENVIRONMENT", "production")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	c, err := authz.NewClient(cfg.AuthzEnv, cfg.AuthzServiceURL, zap.NewNop())
	if err == nil {
		t.Fatalf("production with default AUTHZ_SERVICE_URL (%q) must refuse to build a client, got %T",
			cfg.AuthzServiceURL, c)
	}
}
