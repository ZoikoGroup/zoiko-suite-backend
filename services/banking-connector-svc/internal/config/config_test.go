package config

import "testing"

// TOKENIZATION_ENFORCED is a protection flag, so it is parsed strictly: a
// value that is neither "true" nor "false" refuses to start rather than being
// read as false and storing raw account numbers.
func TestValidate_TokenizationFlagIsStrict(t *testing.T) {
	for value, wantErr := range map[string]bool{"": false, "false": false, "true": false, "True": true, "1": true, "yes": true} {
		t.Setenv("TOKENIZATION_ENFORCED", value)
		cfg := Load()
		if err := cfg.Validate(); (err != nil) != wantErr {
			t.Errorf("TOKENIZATION_ENFORCED=%q: Validate() = %v, want error=%v", value, err, wantErr)
		}
		if cfg.TokenizationEnforced != (value == "true") {
			t.Errorf("TOKENIZATION_ENFORCED=%q: enforced=%v", value, cfg.TokenizationEnforced)
		}
	}
}

func TestLoad_TokenizationDefaultsOffAndPointsAtKeyManagement(t *testing.T) {
	t.Setenv("TOKENIZATION_ENFORCED", "")
	t.Setenv("TOKENIZATION_SERVICE_URL", "")
	cfg := Load()
	if cfg.TokenizationEnforced || cfg.TokenizationServiceURL != "http://key-management-svc:8143" {
		t.Fatalf("defaults: enforced=%v url=%q", cfg.TokenizationEnforced, cfg.TokenizationServiceURL)
	}
}
