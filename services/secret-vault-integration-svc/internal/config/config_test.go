package config

import (
	"strings"
	"testing"
)

func TestValidateInbound_RestrictedEnvRefusesServerOnlyTLS(t *testing.T) {
	// Server-only TLS used to satisfy the production gate: encrypted, but it
	// authenticated no caller — the audit's "no inbound mTLS".
	c := &Config{Env: "production", TLSCertFile: "c.pem", TLSKeyFile: "k.pem"}
	err := c.ValidateInbound()
	if err == nil || !strings.Contains(err.Error(), "mutual TLS") {
		t.Fatalf("server-only TLS in production must be refused, got %v", err)
	}
}

func TestValidateInbound_RestrictedEnvRefusesPlainHTTP(t *testing.T) {
	if err := (&Config{Env: "staging"}).ValidateInbound(); err == nil {
		t.Fatal("plain HTTP in staging must be refused")
	}
}

func TestValidateInbound_RestrictedEnvRefusesIdentityCheckOff(t *testing.T) {
	c := &Config{Env: "production", TLSCertFile: "c", TLSKeyFile: "k", TLSClientCAFile: "ca", MTLSIdentityCheck: false}
	if err := c.ValidateInbound(); err == nil {
		t.Fatal("mTLS with the identity check off must be refused in production")
	}
}

func TestValidateInbound_MutualTLSWithIdentityCheckAccepted(t *testing.T) {
	c := &Config{Env: "production", TLSCertFile: "c", TLSKeyFile: "k", TLSClientCAFile: "ca", MTLSIdentityCheck: true}
	if err := c.ValidateInbound(); err != nil {
		t.Fatalf("full mTLS must be accepted: %v", err)
	}
}

func TestValidateInbound_OverrideAndLocal(t *testing.T) {
	if err := (&Config{Env: "production", AllowInsecureInbound: true}).ValidateInbound(); err != nil {
		t.Fatalf("ALLOW_INSECURE_INBOUND must remain the documented override: %v", err)
	}
	if err := (&Config{Env: "local"}).ValidateInbound(); err != nil {
		t.Fatalf("local plain HTTP must still start: %v", err)
	}
}

func TestValidateInbound_CAWithoutKeypairRefusedEverywhere(t *testing.T) {
	if err := (&Config{Env: "local", TLSClientCAFile: "ca"}).ValidateInbound(); err == nil {
		t.Fatal("a client CA without a server keypair silently served plain HTTP; must be refused")
	}
}

func TestIdentityCheckDefault(t *testing.T) {
	cases := []struct {
		explicit, ca string
		want         bool
	}{
		{"", "", false},
		{"", "ca.pem", true}, // on by default with mTLS
		{"false", "ca.pem", false},
		{"true", "", true},
	}
	for _, c := range cases {
		if got := identityCheckDefault(c.explicit, c.ca); got != c.want {
			t.Errorf("identityCheckDefault(%q,%q) = %v, want %v", c.explicit, c.ca, got, c.want)
		}
	}
}

func TestValidateVault_InProcessKeyRefusedInRestrictedEnv(t *testing.T) {
	for _, e := range []string{"production", "staging"} {
		if err := (&Config{Env: e, VaultKEKProvider: "local"}).ValidateVault(); err == nil {
			t.Fatalf("an in-process master key must be refused in %s", e)
		}
	}
	if err := (&Config{Env: "production", VaultKEKProvider: "local", AllowInProcessMasterKey: true}).ValidateVault(); err != nil {
		t.Fatalf("the documented override must still start: %v", err)
	}
	if err := (&Config{Env: "local", VaultKEKProvider: "local"}).ValidateVault(); err != nil {
		t.Fatalf("local dev keeps the local key: %v", err)
	}
}

func TestValidateVault_KMSProvidersNeedTheirSettings(t *testing.T) {
	if err := (&Config{VaultKEKProvider: "transit"}).ValidateVault(); err == nil {
		t.Fatal("transit without address/key/token must be refused")
	}
	if err := (&Config{VaultKEKProvider: "gcpkms"}).ValidateVault(); err == nil {
		t.Fatal("gcpkms without a key name must be refused")
	}
	if err := (&Config{VaultKEKProvider: "kms-of-my-choosing"}).ValidateVault(); err == nil {
		t.Fatal("an unknown provider must be refused, not defaulted")
	}
	ok := &Config{Env: "production", VaultKEKProvider: "gcpkms", GCPKMSKeyName: "projects/p/locations/l/keyRings/r/cryptoKeys/k"}
	if err := ok.ValidateVault(); err != nil {
		t.Fatalf("gcpkms in production must be accepted: %v", err)
	}
}

func TestValidateLeaseCeiling(t *testing.T) {
	if err := (&Config{Env: "production", MaxLeaseDurationSeconds: 0}).ValidateLeaseCeiling(); err == nil {
		t.Fatal("no lease ceiling must be refused in production")
	}
	if err := (&Config{Env: "local", MaxLeaseDurationSeconds: 0}).ValidateLeaseCeiling(); err != nil {
		t.Fatalf("local may run without a ceiling: %v", err)
	}
	if err := (&Config{Env: "production", MaxLeaseDurationSeconds: 3600}).ValidateLeaseCeiling(); err != nil {
		t.Fatalf("a positive ceiling is accepted: %v", err)
	}
}
