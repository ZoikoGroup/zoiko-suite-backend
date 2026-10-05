package config

import (
	"strings"
	"testing"
	"time"

	"zoiko.io/notification-svc/internal/webhook"
)

func TestMinWebhookSecretMatchesTheVerifier(t *testing.T) {
	if minWebhookSecret != webhook.MinSecretLength {
		t.Fatalf("config accepts secrets of %d bytes but the verifier needs %d: a secret that passes config would be silently unusable",
			minWebhookSecret, webhook.MinSecretLength)
	}
}

func TestParseWebhookSecrets(t *testing.T) {
	long := strings.Repeat("s", 16)
	got, err := parseWebhookSecrets(`{"ses":["` + long + `","` + long + `2"],"sendgrid":["` + long + `"]}`)
	if err != nil || len(got["ses"]) != 2 || len(got["sendgrid"]) != 1 {
		t.Fatalf("valid secrets rejected or mis-parsed: %v %v", got, err)
	}
	if m, err := parseWebhookSecrets("  "); err != nil || m != nil {
		t.Fatalf("empty is valid and means no provider may call back: %v %v", m, err)
	}
	for name, raw := range map[string]string{
		"not json":        `ses=abc`,
		"wrong shape":     `{"ses":"` + long + `"}`,
		"short secret":    `{"ses":["short"]}`,
		"empty list":      `{"ses":[]}`,
		"empty provider":  `{"":["` + long + `"]}`,
		"mixed good, bad": `{"ses":["` + long + `","x"]}`,
	} {
		if _, err := parseWebhookSecrets(raw); err == nil {
			t.Errorf("%s: accepted %q", name, raw)
		}
	}
}

func TestLoadRefusesMalformedWebhookSecretsAndDefaultsTolerance(t *testing.T) {
	t.Setenv("NOTIFICATION_WEBHOOK_SECRETS", `{"ses":["short"]}`)
	if _, err := Load(); err == nil {
		t.Fatal("Load must refuse a malformed secret list rather than silently close the ingress")
	}
	t.Setenv("NOTIFICATION_WEBHOOK_SECRETS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookTolerance != 5*time.Minute || len(cfg.WebhookSecrets) != 0 {
		t.Fatalf("defaults wrong: tolerance=%v secrets=%v", cfg.WebhookTolerance, cfg.WebhookSecrets)
	}
}

func TestLedgerRegisterIsOffByDefault(t *testing.T) {
	t.Setenv("NOTIFICATION_WEBHOOK_SECRETS", "")
	t.Setenv("NOTIFICATION_LEDGER_REGISTER_ENABLED", "")
	cfg, err := Load()
	if err != nil || cfg.LedgerRegisterEnabled {
		t.Fatalf("the register link writes twice and must be opt-in: enabled=%v err=%v", cfg != nil && cfg.LedgerRegisterEnabled, err)
	}
	t.Setenv("NOTIFICATION_LEDGER_REGISTER_ENABLED", "true")
	if cfg, err = Load(); err != nil || !cfg.LedgerRegisterEnabled {
		t.Fatalf("explicit opt-in must enable it: %v", err)
	}
}
