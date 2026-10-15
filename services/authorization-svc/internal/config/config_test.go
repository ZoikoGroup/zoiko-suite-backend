package config_test

import (
	"testing"

	"zoiko.io/authorization-svc/internal/config"
)

// §16 reason_code is enforced unless a deployment explicitly opts back to
// warn: a manifest that never mentions AUTHZ_COMMAND_CONTRACT must not
// silently admit reasonless destructive commands.
func TestCommandContract_DefaultsToEnforce(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", true}, {"enforce", true}, {"ENFORCE", true}, {"warn", false}, {"WARN", false}} {
		t.Setenv("AUTHZ_COMMAND_CONTRACT", tc.value)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.CommandContractEnforce != tc.want {
			t.Errorf("AUTHZ_COMMAND_CONTRACT=%q: enforce = %v, want %v", tc.value, cfg.CommandContractEnforce, tc.want)
		}
	}
}

func TestEntityRegistryURL_FromEnv(t *testing.T) {
	t.Setenv("AUTHZ_ENTITY_REGISTRY_URL", "http://tenant-entity-registry-svc:8081")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EntityRegistryURL != "http://tenant-entity-registry-svc:8081" {
		t.Errorf("EntityRegistryURL = %q", cfg.EntityRegistryURL)
	}
}
