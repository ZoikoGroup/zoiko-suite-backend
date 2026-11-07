package config

import (
	"strings"
	"testing"
)

func TestConfig_CursorKeyRequiredInProtectedTiers(t *testing.T) {
	for _, env := range []string{"staging", "production"} {
		if _, err := cursorSigningKey("", env); err == nil || !strings.Contains(err.Error(), "CURSOR_SIGNING_KEY_HEX") {
			t.Fatalf("%s: want CURSOR_SIGNING_KEY_HEX refusal, got %v", env, err)
		}
	}
	if _, err := cursorSigningKey("00ff", "production"); err == nil {
		t.Fatal("a 2-byte key must be refused")
	}
	if _, err := cursorSigningKey("zz", "local"); err == nil {
		t.Fatal("a non-hex key must be refused")
	}
	key, err := cursorSigningKey("", "local")
	if err != nil || len(key) != 32 {
		t.Fatalf("local generates a 32-byte key, got %d bytes, err %v", len(key), err)
	}
}

func TestConfig_CommercialAccountURLRequiredOutsideLocal(t *testing.T) {
	mfaBaseEnv(t)
	t.Setenv("MFA_SECRET_KEY", testMFAKey)
	t.Setenv("DEPLOY_ENVIRONMENT", "production")
	t.Setenv("COMMERCIAL_ACCOUNT_URL", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "COMMERCIAL_ACCOUNT_URL") {
		t.Fatalf("want COMMERCIAL_ACCOUNT_URL refusal in production, got %v", err)
	}
	t.Setenv("DEPLOY_ENVIRONMENT", "local")
	if _, err := Load(); err != nil && strings.Contains(err.Error(), "COMMERCIAL_ACCOUNT_URL") {
		t.Fatalf("local must start without COMMERCIAL_ACCOUNT_URL: %v", err)
	}
}
