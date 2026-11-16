package config

import (
	"strings"
	"testing"
)

func TestSecretsConfig_AVaultPathNeedsTheVaultIdentity(t *testing.T) {
	if err := (SecretsConfig{}).validate(); err != nil {
		t.Fatalf("no vault source is fine: %v", err)
	}
	s := SecretsConfig{SMTPPasswordPath: "kv/smtp"}
	if err := s.validate(); err == nil {
		t.Fatal("a path without a vault URL, principal and tenant cannot work and must be refused")
	}
	s.VaultURL, s.Principal, s.TenantID = "http://vault:8087", "notification-svc", "scope-1"
	if err := s.validate(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestSecretsConfig_ProductionRefusesEnvSecretsUnlessOverridden(t *testing.T) {
	c := &Config{}
	c.Email.Password = "pw"
	c.SecondaryEmail.Password = "pw2"
	c.WebhookSecrets = map[string][]string{"ses": {"0123456789abcdef"}}
	got := strings.Join(c.secretsProductionProblems(), " | ")
	for _, want := range []string{"SMTP_PASSWORD", "SMTP_SECONDARY_PASSWORD", "NOTIFICATION_WEBHOOK_SECRETS"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s must be named: %s", want, got)
		}
	}
	c.Secrets = SecretsConfig{SMTPPasswordPath: "a", SMTPSecondaryPasswordPath: "b", WebhookSecretsPath: "c"}
	if p := c.secretsProductionProblems(); len(p) != 0 {
		t.Fatalf("vault-sourced secrets are fine: %v", p)
	}
	c.Secrets = SecretsConfig{AllowEnvSecrets: true}
	if p := c.secretsProductionProblems(); len(p) != 0 {
		t.Fatalf("the override is honoured: %v", p)
	}
	empty := &Config{}
	if p := empty.secretsProductionProblems(); len(p) != 0 {
		t.Fatalf("nothing set, nothing to refuse: %v", p)
	}
}
