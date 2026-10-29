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
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Environment, c.RetentionSet = "production", false
	c.ModelProvider = "mock" // isolate the retention rule from the provider rule
	if err := validateRetention(c); err == nil {
		t.Fatal("production without PUBLIC_AI_RETENTION must refuse to start")
	}
	c.RetentionSet = true
	if err := validateRetention(c); err != nil {
		t.Fatalf("explicit retention should pass: %v", err)
	}
}

// Master prompt STEP 2: only the mock provider exists, only in dev/test, and
// the model id must be allowlisted.
func TestModelProviderRules(t *testing.T) {
	if c, err := Load(); err != nil || c.ModelProvider != "mock" || c.HTTPAddr != "127.0.0.1:8001" {
		t.Fatalf("dev defaults: %+v %v", c.ModelProvider, err)
	}
	t.Setenv("MODEL_PROVIDER", "openai")
	if _, err := Load(); err == nil {
		t.Fatal("an unbuilt provider must refuse to start")
	}
	t.Setenv("MODEL_PROVIDER", "mock")
	t.Setenv("PUBLIC_AI_MODEL_ID", "gpt-unlisted")
	if _, err := Load(); err == nil {
		t.Fatal("a model id outside the allowlist must refuse to start")
	}
	t.Setenv("PUBLIC_AI_MODEL_ID", "")
	t.Setenv("PUBLIC_AI_ENVIRONMENT", "production")
	t.Setenv("PUBLIC_AI_RETENTION", "720h")
	if _, err := Load(); err == nil {
		t.Fatal("the mock provider must not start in production")
	}
}

// MODEL_PROVIDER=groq needs a server-side key and an allowlisted model.
func TestGroqProviderRules(t *testing.T) {
	t.Setenv("MODEL_PROVIDER", "groq")
	t.Setenv("GROQ_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("groq without GROQ_API_KEY must refuse to start")
	}
	t.Setenv("GROQ_API_KEY", "gsk_test_not_a_real_key")
	c, err := Load()
	if err != nil || c.ModelID != "openai/gpt-oss-120b" {
		t.Fatalf("groq defaults: %q %v", c.ModelID, err)
	}
	t.Setenv("GROQ_MODEL", "some/unlisted-model")
	if _, err := Load(); err == nil {
		t.Fatal("a GROQ_MODEL outside the allowlist must refuse to start")
	}
}

func TestTrustedProxiesValidated(t *testing.T) {
	for _, v := range []string{"localhost", "0.0.0.0/0", "::/0"} {
		t.Setenv("PUBLIC_AI_TRUSTED_PROXIES", v)
		if _, err := Load(); err == nil {
			t.Errorf("PUBLIC_AI_TRUSTED_PROXIES=%s must be refused", v)
		}
	}
	t.Setenv("PUBLIC_AI_TRUSTED_PROXIES", "127.0.0.1/32, 10.0.0.0/8")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
