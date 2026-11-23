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
	t.Setenv("IDP_CLIENT_AUTH", "required")
	t.Setenv("IDP_CLIENT_SECRETS_FILE", "/fake/secrets.json")
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

// IC-FE-4 / S1-B1 (D1-1.F29): outside local the console must sign in as a
// confidential client. IDP_CLIENT_AUTH=optional is refused at boot in every
// tier but local.
func TestConfig_IDPClientAuthMustBeRequiredOutsideLocal(t *testing.T) {
	for _, envName := range []string{"development", "staging", "production"} {
		t.Run(envName, func(t *testing.T) {
			mfaBaseEnv(t)
			t.Setenv("MFA_SECRET_KEY", testMFAKey)
			t.Setenv("COMMERCIAL_ACCOUNT_URL", "http://commercial-account-svc:8150")
			t.Setenv("DEPLOY_ENVIRONMENT", envName)
			t.Setenv("IDP_CLIENT_AUTH", "optional")
			t.Setenv("IDP_CLIENT_SECRETS_FILE", "")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "IDP_CLIENT_AUTH must be 'required'") {
				t.Fatalf("IDP_CLIENT_AUTH=optional in %s: want the refusal, got %v", envName, err)
			}
			t.Setenv("IDP_CLIENT_AUTH", "required")
			t.Setenv("IDP_CLIENT_SECRETS_FILE", "/fake/secrets.json")
			if _, err := Load(); err != nil && strings.Contains(err.Error(), "IDP_CLIENT_AUTH") {
				t.Fatalf("required with a secrets file must pass this rule in %s: %v", envName, err)
			}
		})
	}
	mfaBaseEnv(t)
	t.Setenv("DEPLOY_ENVIRONMENT", "local")
	t.Setenv("IDP_CLIENT_AUTH", "optional")
	if _, err := Load(); err != nil && strings.Contains(err.Error(), "IDP_CLIENT_AUTH") {
		t.Fatalf("local may run optional: %v", err)
	}
}
