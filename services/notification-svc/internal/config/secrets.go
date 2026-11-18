package config

import (
	"errors"
	"time"
)

// SecretsConfig sources provider and callback secrets from secret-vault-integration-svc instead of the environment
// (S7-B3, ZS-SVC-Y-001 INV-26). Each *Path names the vault path of a policy of class PROVIDER_CREDENTIAL (the SMTP
// passwords) or CALLBACK_SECRET (the webhook secret list, a JSON object of provider to list of secrets, as in
// NOTIFICATION_WEBHOOK_SECRETS). A path set means the vault is the source: the environment value is ignored.
//
// The action-token and unsubscribe HMAC keys and the DKIM private key are not covered; see package vaultsecrets.
type SecretsConfig struct {
	// VaultURL is secret-vault-integration-svc. Principal is this service's workload identity, which every policy must
	// list in its allowed workloads; TenantID is the scope those policies are registered under.
	VaultURL  string
	Principal string
	TenantID  string

	SMTPPasswordPath          string
	SMTPSecondaryPasswordPath string
	WebhookSecretsPath        string

	// RefreshInterval is how often the secrets are re-fetched so a rotation reaches the running process. Zero means
	// they are read once, at start-up.
	RefreshInterval time.Duration

	// AllowEnvSecrets lets production keep a provider or callback secret in the environment. Off by default: it is the
	// documented override, not the way to run.
	AllowEnvSecrets bool
}

// Enabled reports whether any secret is sourced from the vault.
func (s SecretsConfig) Enabled() bool {
	return s.SMTPPasswordPath != "" || s.SMTPSecondaryPasswordPath != "" || s.WebhookSecretsPath != ""
}

func loadSecretsConfig() SecretsConfig {
	return SecretsConfig{
		VaultURL:                  env("NOTIFICATION_SECRETS_VAULT_URL", ""),
		Principal:                 env("NOTIFICATION_SECRETS_PRINCIPAL", "notification-svc"),
		TenantID:                  env("NOTIFICATION_SECRETS_TENANT_ID", ""),
		SMTPPasswordPath:          env("NOTIFICATION_SMTP_PASSWORD_SECRET_PATH", ""),
		SMTPSecondaryPasswordPath: env("NOTIFICATION_SMTP_SECONDARY_PASSWORD_SECRET_PATH", ""),
		WebhookSecretsPath:        env("NOTIFICATION_WEBHOOK_SECRETS_SECRET_PATH", ""),
		RefreshInterval:           envDuration("NOTIFICATION_SECRETS_REFRESH_INTERVAL", 10*time.Minute),
		AllowEnvSecrets:           env("NOTIFICATION_ALLOW_ENV_SECRETS", "false") == "true",
	}
}

// validate refuses a vault source that cannot work, in every environment.
func (s SecretsConfig) validate() error {
	if !s.Enabled() {
		return nil
	}
	if s.VaultURL == "" || s.TenantID == "" || s.Principal == "" {
		return errors.New("a vault secret path is set but NOTIFICATION_SECRETS_VAULT_URL, NOTIFICATION_SECRETS_TENANT_ID or NOTIFICATION_SECRETS_PRINCIPAL is not")
	}
	return nil
}

// productionProblems lists provider and callback secrets production would still read from the environment.
func (c *Config) secretsProductionProblems() []string {
	if c.Secrets.AllowEnvSecrets {
		return nil
	}
	var out []string
	if c.Email.Password != "" && c.Secrets.SMTPPasswordPath == "" {
		out = append(out, "SMTP_PASSWORD (lease it from the vault with NOTIFICATION_SMTP_PASSWORD_SECRET_PATH, or set NOTIFICATION_ALLOW_ENV_SECRETS=true)")
	}
	if c.SecondaryEmail.Password != "" && c.Secrets.SMTPSecondaryPasswordPath == "" {
		out = append(out, "SMTP_SECONDARY_PASSWORD (lease it from the vault with NOTIFICATION_SMTP_SECONDARY_PASSWORD_SECRET_PATH, or set NOTIFICATION_ALLOW_ENV_SECRETS=true)")
	}
	if len(c.WebhookSecrets) > 0 && c.Secrets.WebhookSecretsPath == "" {
		out = append(out, "NOTIFICATION_WEBHOOK_SECRETS (lease it from the vault with NOTIFICATION_WEBHOOK_SECRETS_SECRET_PATH, or set NOTIFICATION_ALLOW_ENV_SECRETS=true)")
	}
	return out
}

// ParseWebhookSecrets parses a webhook secret list (provider to a list of secrets), refusing a malformed one. The same
// rules apply whether the list came from the environment or the vault.
func ParseWebhookSecrets(raw string) (map[string][]string, error) { return parseWebhookSecrets(raw) }
