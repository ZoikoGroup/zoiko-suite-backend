package config

import (
	"strings"
	"testing"
)

const testMFAKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=" // 32 bytes

func mfaBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SIGNING_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("SOD_SERVICE_URL", "http://sod-svc:8090")
	t.Setenv("AUTHZ_ENV", "")
	t.Setenv("MFA_SECRET_KEY", "")
	t.Setenv("MFA_SECRET_KEY_ID", "")
	t.Setenv("MFA_SECRET_PREVIOUS_KEYS", "")
	t.Setenv("IDP_CLIENT_AUTH", "")
	t.Setenv("IDP_CLIENT_SECRETS_FILE", "")
	t.Setenv("MFA_TOTP_SKEW_STEPS", "")
	t.Setenv("OIDC_ISSUER", "https://auth.example.com")
}

func TestMFAKey_RequiredInProtectedTiers(t *testing.T) {
	for _, env := range []string{"staging", "production"} {
		t.Run(env, func(t *testing.T) {
			mfaBaseEnv(t)
			t.Setenv("DEPLOY_ENVIRONMENT", env)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "MFA_SECRET_KEY") {
				t.Fatalf("want MFA_SECRET_KEY refusal, got %v", err)
			}
			t.Setenv("MFA_SECRET_KEY", testMFAKey)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.MFASecretKeys["k1"]) != 32 || cfg.MFASecretKeyID != "k1" {
				t.Fatalf("keyring not loaded: %v %s", len(cfg.MFASecretKeys), cfg.MFASecretKeyID)
			}
		})
	}
}

func TestMFAKey_OptionalLocallyAndValidated(t *testing.T) {
	mfaBaseEnv(t)
	t.Setenv("DEPLOY_ENVIRONMENT", "local")
	cfg, err := Load()
	if err != nil || len(cfg.MFASecretKeys) != 0 {
		t.Fatalf("local without key: %v keys=%d", err, len(cfg.MFASecretKeys))
	}
	t.Setenv("MFA_SECRET_KEY", "c2hvcnQ=") // "short"
	if _, err := Load(); err == nil {
		t.Fatal("a key that is not 32 bytes was accepted")
	}
	t.Setenv("MFA_SECRET_KEY", "")
	t.Setenv("MFA_SECRET_PREVIOUS_KEYS", "k0:"+testMFAKey)
	if _, err := Load(); err == nil {
		t.Fatal("previous keys without a current key were accepted")
	}
	t.Setenv("MFA_SECRET_KEY", testMFAKey)
	t.Setenv("MFA_SECRET_KEY_ID", "k2")
	cfg, err = Load()
	if err != nil || len(cfg.MFASecretKeys) != 2 || cfg.MFASecretKeyID != "k2" {
		t.Fatalf("rotation ring: %v keys=%d id=%s", err, len(cfg.MFASecretKeys), cfg.MFASecretKeyID)
	}
	t.Setenv("MFA_TOTP_SKEW_STEPS", "3")
	if _, err := Load(); err == nil {
		t.Fatal("skew 3 accepted")
	}
}

func TestIDPClientAuth_Validated(t *testing.T) {
	mfaBaseEnv(t)
	t.Setenv("DEPLOY_ENVIRONMENT", "local")
	t.Setenv("IDP_CLIENT_AUTH", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("invalid IDP_CLIENT_AUTH accepted")
	}
	t.Setenv("IDP_CLIENT_AUTH", "required")
	if _, err := Load(); err == nil {
		t.Fatal("required client auth with no registry accepted")
	}
}
