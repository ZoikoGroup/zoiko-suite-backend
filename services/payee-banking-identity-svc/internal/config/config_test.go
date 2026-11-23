package config

import "testing"

// TOKENIZATION_ENFORCED is a protection flag, so it is parsed strictly: a
// value that is neither "true" nor "false" refuses to start rather than being
// read as false and storing raw account identifiers.
func TestLoad_TokenizationFlagIsStrict(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	for value, wantErr := range map[string]bool{"": false, "false": false, "true": false, "True": true, "1": true, "yes": true} {
		t.Setenv("TOKENIZATION_ENFORCED", value)
		cfg, err := Load()
		if (err != nil) != wantErr {
			t.Errorf("TOKENIZATION_ENFORCED=%q: Load() error = %v, want error=%v", value, err, wantErr)
			continue
		}
		if err == nil && cfg.TokenizationEnforced != (value == "true") {
			t.Errorf("TOKENIZATION_ENFORCED=%q: enforced=%v", value, cfg.TokenizationEnforced)
		}
	}
}

func TestLoad_TokenizationDefaultsOffAndPointsAtKeyManagement(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("TOKENIZATION_ENFORCED", "")
	t.Setenv("TOKENIZATION_SERVICE_URL", "")
	cfg, err := Load()
	if err != nil || cfg.TokenizationEnforced || cfg.TokenizationServiceURL != "http://key-management-svc:8143" {
		t.Fatalf("defaults: %+v, %v", cfg, err)
	}
}
