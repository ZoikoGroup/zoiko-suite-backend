package config

import (
	"strings"
	"testing"
)

// The legacy maker-checker switch re-opens exactly the defect migration 000007
// closes — a maker naming their own approver — so it must be impossible to
// start with it anywhere maker-checker has to mean something.
func TestLoad_RefusesLegacyBodyApproverInStagingAndProduction(t *testing.T) {
	for _, env := range []string{"production", "staging", "Production"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("ENV", env)
			t.Setenv("DB_PASSWORD", "x")
			t.Setenv("DB_SSLMODE", "require")
			t.Setenv("AUTHZ_PLATFORM_SCOPE_ID", "platform-scope")
			t.Setenv("MAKER_CHECKER_LEGACY_BODY_APPROVER", "true")

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "MAKER_CHECKER_LEGACY_BODY_APPROVER") {
				t.Fatalf("Load() = %v, want refusal of the legacy approver in %s", err, env)
			}
		})
	}
}

func TestLoad_LegacyBodyApproverIsOffByDefaultAndAllowedLocally(t *testing.T) {
	t.Setenv("ENV", "local")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.MakerCheckerLegacyBodyApprover {
		t.Fatal("legacy approver must default to off")
	}
	if cfg.ApprovalTTLHours != 168 {
		t.Fatalf("ApprovalTTLHours = %d, want 168", cfg.ApprovalTTLHours)
	}

	t.Setenv("MAKER_CHECKER_LEGACY_BODY_APPROVER", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with legacy locally: %v", err)
	}
	if !cfg.MakerCheckerLegacyBodyApprover {
		t.Fatal("legacy approver should be honoured locally")
	}
}

func TestLoad_RefusesANonPositiveApprovalTTL(t *testing.T) {
	t.Setenv("ENV", "local")
	t.Setenv("APPROVAL_TTL_HOURS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("a zero TTL would expire every approval at birth")
	}
}

// The two 000008 migration aids re-open a closed gap each (entities that skip
// verification; tenants that can be duplicated), so neither may start outside
// local development.
func TestLoad_RefusesThe000008CompatibilityFlagsInProduction(t *testing.T) {
	for _, flag := range []string{"LEGACY_ENTITY_CREATE_ACTIVE", "ONBOARDING_KEY_OPTIONAL"} {
		t.Run(flag, func(t *testing.T) {
			t.Setenv("ENV", "production")
			t.Setenv("DB_PASSWORD", "x")
			t.Setenv("AUTHZ_PLATFORM_SCOPE_ID", "platform-scope")
			t.Setenv(flag, "true")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), flag) {
				t.Fatalf("Load() = %v, want refusal of %s", err, flag)
			}
		})
	}
}
