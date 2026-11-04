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
	if err := c.Validate(); err != nil {
		t.Fatalf("valid production config refused: %v", err)
	}
}
