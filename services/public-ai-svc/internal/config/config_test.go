package config

import "testing"

func TestDefaultsAreValid(t *testing.T) {
	t.Setenv("PUBLIC_AI_ALLOWED_ORIGINS", "http://localhost:3000, https://www.example.invalid")
	c, err := Load()
	if err != nil {
		t.Fatalf("default config should load: %v", err)
	}
	if len(c.AllowedOrigins) != 2 {
		t.Fatalf("origins not split: %v", c.AllowedOrigins)
	}
	if c.Enabled {
		t.Fatal("PUBLIC_AI_ENABLED must default to false")
	}
}

// SEC-T039 precondition: raw prompt logging cannot be switched on.
func TestRawPromptLoggingRefused(t *testing.T) {
	t.Setenv("PUBLIC_AI_LOG_RAW_PROMPT", "true")
	if _, err := Load(); err == nil {
		t.Fatal("expected refusal when PUBLIC_AI_LOG_RAW_PROMPT=true")
	}
}

func TestWildcardOriginRefused(t *testing.T) {
	t.Setenv("PUBLIC_AI_ALLOWED_ORIGINS", "*")
	if _, err := Load(); err == nil {
		t.Fatal("expected refusal of wildcard origin")
	}
}

func TestSingleActiveTurnEnforced(t *testing.T) {
	t.Setenv("PUBLIC_AI_MAX_ACTIVE_TURNS_PER_SESSION", "2")
	if _, err := Load(); err == nil {
		t.Fatal("expected refusal of >1 active turn per session")
	}
}

// Master prompt §10: retention is never a guessed default outside dev/test.
func TestRetentionRequiredOutsideDev(t *testing.T) {
	t.Setenv("PUBLIC_AI_ENVIRONMENT", "production")
	t.Setenv("PUBLIC_AI_RETENTION", "")
	if _, err := Load(); err == nil {
		t.Fatal("production without PUBLIC_AI_RETENTION must refuse to start")
	}
	t.Setenv("PUBLIC_AI_RETENTION", "720h")
	if _, err := Load(); err != nil {
		t.Fatalf("explicit retention should load: %v", err)
	}
}
