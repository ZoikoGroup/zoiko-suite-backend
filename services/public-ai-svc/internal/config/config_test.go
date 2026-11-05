package config

import (
	"os"
	"strings"
	"testing"
)

// Tests load a dev config unless they say otherwise: the environment is never
// assumed by the service itself (SEC-P0-06).
func TestMain(m *testing.M) {
	_ = os.Setenv("PUBLIC_AI_ENVIRONMENT", "dev")
	os.Exit(m.Run())
}

// SEC-P0-06: an unset or unknown environment refuses to start.
func TestEnvironmentFailsClosed(t *testing.T) {
	for _, env := range []string{"", "prod", "development"} {
		t.Setenv("PUBLIC_AI_ENVIRONMENT", env)
		if _, err := Load(); err == nil {
			t.Errorf("PUBLIC_AI_ENVIRONMENT=%q must refuse to start", env)
		}
	}
}

// Doc 06 §08 and SEC-P0-06: outside dev/test the approved policy digest is
// required, and a remote provider needs its approval reference and region.
func TestProductionGates(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Environment, c.RetentionSet, c.ModelProvider, c.GroqAPIKey = "production", true, "groq", "k"
	c.ModelID = c.AllowedModelIDs[0]
	c.KBAllowDraft, c.StructuredResponseDev = false, false
	if err := c.Validate(); err == nil {
		t.Fatal("production without a policy digest must refuse to start")
	}
	c.PolicyDigest = "abc"
	if err := c.Validate(); err == nil {
		t.Fatal("a remote provider without approval must refuse to start")
	}
	c.ProviderApprovalRef, c.ProviderRegion, c.ApprovedRegions = "DPA-2026-01", "eu-west", []string{"eu-west"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ALLOWED_HOSTS") {
		t.Fatalf("production without a Host allowlist must refuse to start: %v", err)
	}
	c.AllowedHosts = []string{"assistant.example.test"}
	if err := c.Validate(); err != nil && containsAny(err.Error(), "POLICY_DIGEST", "PROVIDER_APPROVAL_REF", "ALLOWED_HOSTS") {
		t.Fatalf("all gates satisfied but still refused for them: %v", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestUnknownProviderRejectedBeforeApprovalGates(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Environment, c.ModelProvider, c.DatabaseURL = "production", "openai", "postgres://example"
	err = c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("unknown provider must fail on the provider name first: %v", err)
	}
	if strings.Contains(err.Error(), "APPROVAL_REF") || strings.Contains(err.Error(), "APPROVED_REGIONS") {
		t.Fatalf("provider typo was reported as an approval problem: %v", err)
	}
}

func TestNonDevRequiresDatabase(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Environment, c.RetentionSet = "production", true
	c.ModelProvider, c.GroqAPIKey = "groq", "test-key"
	c.ModelID = c.AllowedModelIDs[0]
	c.PolicyDigest = "policy-digest"
	c.AllowedHosts = []string{"assistant.example.test"}
	c.ProviderApprovalRef, c.ProviderRegion, c.ApprovedRegions = "DPA-2026-01", "eu-west", []string{"eu-west"}
	c.KBAllowDraft, c.SeedOnStart, c.StructuredResponseDev = false, false, false
	c.DatabaseURL = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PUBLIC_AI_DATABASE_URL") {
		t.Fatalf("non-dev must refuse an in-memory fallback: %v", err)
	}
	c.DatabaseURL = "postgres://example/public_ai"
	c.AllowedOrigins = []string{"https://www.zoikosuite.example"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PUBLIC_AI_ADMIN_ADDR") {
		t.Fatalf("non-dev must keep /metrics and /health off the public listener: %v", err)
	}
	c.AdminAddr = ":9091"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "PUBLIC_AI_RATE_KEY_SALT") {
		t.Fatalf("non-dev must share rate limits across replicas: %v", err)
	}
	c.RateKeySalt = strings.Repeat("s", 32)
	if err := c.Validate(); err != nil {
		t.Fatalf("valid production config refused: %v", err)
	}
}

// validProduction is a production config that passes every gate.
func validProduction(t *testing.T) Config {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Environment, c.RetentionSet = "production", true
	c.ModelProvider, c.GroqAPIKey = "groq", "test-key"
	c.ModelID = c.AllowedModelIDs[0]
	c.PolicyDigest = "policy-digest"
	c.AllowedHosts = []string{"assistant.example.test"}
	c.ProviderApprovalRef, c.ProviderRegion, c.ApprovedRegions = "DPA-2026-01", "eu-west", []string{"eu-west"}
	c.KBAllowDraft, c.SeedOnStart, c.StructuredResponseDev = false, false, false
	c.DatabaseURL = "postgres://example/public_ai"
	c.AllowedOrigins = []string{"https://www.zoikosuite.example"}
	c.AdminAddr = ":9091"
	c.RateKeySalt = strings.Repeat("s", 32)
	if err := c.Validate(); err != nil {
		t.Fatalf("baseline production config refused: %v", err)
	}
	return c
}

// SEC-P0-09, D06-09: outside dev/test every limit is set (> 0) and every
// allowed origin is https and not localhost.
func TestProductionLimitsAndOrigins(t *testing.T) {
	zero := map[string]func(*Config){
		"PUBLIC_AI_RATE_LIMIT_IP_PER_MIN":      func(c *Config) { c.RateLimitPerIPPerMinute = 0 },
		"PUBLIC_AI_RATE_LIMIT_SESSION_PER_MIN": func(c *Config) { c.RateLimitPerSessionPerMin = -1 },
		"PUBLIC_AI_DAILY_TOKEN_CEILING":        func(c *Config) { c.DailyTokenCeiling = 0 },
		"PUBLIC_AI_MONTHLY_TOKEN_CEILING":      func(c *Config) { c.MonthlyTokenCeiling = 0 },
		"PUBLIC_AI_SESSION_TOKEN_CAP":          func(c *Config) { c.SessionTokenCap = 0 },
		"PUBLIC_AI_TOKEN_BUDGET_PER_TURN":      func(c *Config) { c.TokenBudgetPerTurn = 0 },
	}
	for name, mut := range zero {
		c := validProduction(t)
		mut(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s <= 0 in production: %v", name, err)
		}
	}
	for _, o := range []string{"http://www.zoikosuite.example", "https://localhost:3000", "https://127.0.0.1", "https://app.localhost", "https://x.example/path", "www.zoikosuite.example"} {
		c := validProduction(t)
		c.AllowedOrigins = []string{"https://www.zoikosuite.example", o}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ALLOWED_ORIGINS") {
			t.Errorf("origin %q in production: %v", o, err)
		}
	}
	// dev keeps the localhost console origin and the dev defaults.
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.RateLimitPerIPPerMinute = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("dev must keep working with localhost origins and unset limits: %v", err)
	}
}

// Doc 08 §11, OPR-012: drain (5 s) + 2 x model timeout + 15 s must fit the
// 45 s stop grace period, so the model timeout is at most 12.5 s.
func TestModelTimeoutFitsStopGrace(t *testing.T) {
	if got := DrainGrace + 2*MaxModelTimeout + ShutdownSlack; got > StopGracePeriod {
		t.Fatalf("bound is wrong: %s > %s", got, StopGracePeriod)
	}
	t.Setenv("PUBLIC_AI_MODEL_TIMEOUT", "12s")
	if _, err := Load(); err != nil {
		t.Fatalf("12s timeout refused: %v", err)
	}
	t.Setenv("PUBLIC_AI_MODEL_TIMEOUT", "13s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "PUBLIC_AI_MODEL_TIMEOUT") {
		t.Fatalf("13s timeout must be refused: %v", err)
	}
}

// Doc 06 §12: the admin listener is optional and never the public address.
func TestAdminAddr(t *testing.T) {
	t.Setenv("PUBLIC_AI_ADMIN_ADDR", "127.0.0.1:9101")
	c, err := Load()
	if err != nil || c.AdminAddr != "127.0.0.1:9101" {
		t.Fatalf("admin addr: %q %v", c.AdminAddr, err)
	}
	c.AdminAddr = c.HTTPAddr
	if err := c.Validate(); err == nil {
		t.Fatal("admin addr equal to the public addr must be refused")
	}
}
