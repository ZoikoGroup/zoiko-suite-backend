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

// prodEnv is a complete, valid production configuration.
func prodEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ENV", "production")
	t.Setenv("DB_PASSWORD", "x")
	t.Setenv("DB_SSLMODE", "require")
	t.Setenv("AUTHZ_PLATFORM_SCOPE_ID", "platform-scope")
	t.Setenv("JURISDICTION_RULES_URL", "http://jurisdiction-svc:8082")
	t.Setenv("COMMERCIAL_ACCOUNT_URL", "http://commercial-account-svc:8144")
	t.Setenv("RESTRICTED_JURISDICTION_CODES", "KP, ir")
}

func TestLoad_ACompleteProductionConfigStarts(t *testing.T) {
	prodEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got := strings.Join(cfg.RestrictedJurisdictionCodes, ","); got != "KP,IR" {
		t.Fatalf("restricted codes = %q, want KP,IR (trimmed, upper-cased)", got)
	}
}

// Each §4.2 provisioning dependency must be real in production: the stubs
// accept every jurisdiction and every subscription.
func TestLoad_RefusesProvisioningStubsInProduction(t *testing.T) {
	for name, tc := range map[string]struct{ key, value, want string }{
		"jurisdiction stub (default url)": {"JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc", "JURISDICTION_RULES_URL"},
		"no entitlement service":          {"COMMERCIAL_ACCOUNT_URL", "", "COMMERCIAL_ACCOUNT_URL"},
		"restricted list unset":           {"RESTRICTED_JURISDICTION_CODES", "", "RESTRICTED_JURISDICTION_CODES"},
		"legacy provisioning inputs":      {"LEGACY_PROVISIONING_INPUTS", "true", "LEGACY_PROVISIONING_INPUTS"},
		"optional expected_version":       {"EXPECTED_VERSION_OPTIONAL", "true", "EXPECTED_VERSION_OPTIONAL"},
	} {
		t.Run(name, func(t *testing.T) {
			prodEnv(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load() = %v, want refusal naming %s", err, tc.want)
			}
		})
	}
}

// NONE is how production states, explicitly, that nothing is restricted.
func TestLoad_RestrictedNoneIsAnExplicitEmptyList(t *testing.T) {
	prodEnv(t)
	t.Setenv("RESTRICTED_JURISDICTION_CODES", "NONE")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.RestrictedJurisdictionCodes) != 0 {
		t.Fatalf("NONE must mean no codes, got %v", cfg.RestrictedJurisdictionCodes)
	}
}
