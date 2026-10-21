package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// WebhookOutConfig configures INT-04 outbound webhooks (ZS-SVC-M-001): the Kafka
// fan-out that turns platform events into deliveries, and the dispatcher that sends
// them. It is off by default because it needs a vault to sign with and an explicit
// list of source topics and permitted destination regions.
type WebhookOutConfig struct {
	Enabled bool

	// SourceTopics are the event topics the fan-out consumer reads. Empty means the
	// consumer does not run (the API and the dispatcher still do).
	SourceTopics  []string
	ConsumerGroup string

	// VaultURL is secret-vault-integration-svc, which signs on this service's behalf
	// (POST /v1/secrets/sign); the signing key never leaves it. SignerPrincipal is the
	// workload identity this service presents there and must be in each signing
	// policy's allowed workloads. VaultMTLS reuses this service's mTLS client identity.
	VaultURL        string
	VaultMTLS       bool
	SignerPrincipal string

	// Egress: regions a destination may be in (required) and ports it may use.
	// AllowHTTP is for local development only and is refused in production.
	AllowedRegions []string
	AllowedPorts   []int
	AllowHTTP      bool

	Workers                   int
	BatchSize                 int
	PollInterval              time.Duration
	RequestTimeout            time.Duration
	RetryBase                 time.Duration
	RetryCap                  time.Duration
	PerDestinationConcurrency int
	BreakerThreshold          int
	BreakerCooldown           time.Duration
	DisableAfterDeadLetters   int
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadWebhookOut reads the settings. When the feature is enabled it refuses to start
// without a vault and a region allowlist, rather than run a sender that can neither
// sign nor say where it may send.
func loadWebhookOut(production bool) (WebhookOutConfig, error) {
	c := WebhookOutConfig{
		Enabled:                   env("NOTIFICATION_WEBHOOK_OUT_ENABLED", "false") == "true",
		SourceTopics:              splitList(os.Getenv("WEBHOOK_SOURCE_TOPICS")),
		ConsumerGroup:             env("WEBHOOK_CONSUMER_GROUP", "notification-svc-webhook-fanout"),
		VaultURL:                  env("WEBHOOK_VAULT_URL", "http://secret-vault-integration-svc:8087"),
		VaultMTLS:                 env("WEBHOOK_VAULT_MTLS_ENABLED", "false") == "true",
		SignerPrincipal:           env("WEBHOOK_SIGNER_PRINCIPAL", "notification-svc"),
		AllowedRegions:            splitList(os.Getenv("WEBHOOK_ALLOWED_REGIONS")),
		AllowHTTP:                 env("WEBHOOK_ALLOW_HTTP", "false") == "true",
		Workers:                   envInt("WEBHOOK_WORKERS", 8),
		BatchSize:                 envInt("WEBHOOK_BATCH_SIZE", 20),
		PollInterval:              envDuration("WEBHOOK_POLL_INTERVAL", 2*time.Second),
		RequestTimeout:            envDuration("WEBHOOK_REQUEST_TIMEOUT", 10*time.Second),
		RetryBase:                 envDuration("WEBHOOK_RETRY_BASE", 30*time.Second),
		RetryCap:                  envDuration("WEBHOOK_RETRY_CAP", time.Hour),
		PerDestinationConcurrency: envInt("WEBHOOK_PER_DESTINATION_CONCURRENCY", 2),
		BreakerThreshold:          envInt("WEBHOOK_BREAKER_THRESHOLD", 5),
		BreakerCooldown:           envDuration("WEBHOOK_BREAKER_COOLDOWN", 5*time.Minute),
		DisableAfterDeadLetters:   envInt("WEBHOOK_DISABLE_AFTER_DEAD_LETTERS", 5),
	}
	for _, p := range splitList(os.Getenv("WEBHOOK_ALLOWED_PORTS")) {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return c, errors.New("WEBHOOK_ALLOWED_PORTS must be a comma-separated list of ports")
		}
		c.AllowedPorts = append(c.AllowedPorts, n)
	}
	if !c.Enabled {
		return c, nil
	}
	if len(c.AllowedRegions) == 0 {
		return c, errors.New("WEBHOOK_ALLOWED_REGIONS is required when outbound webhooks are enabled")
	}
	if strings.TrimSpace(c.VaultURL) == "" || strings.TrimSpace(c.SignerPrincipal) == "" {
		return c, errors.New("WEBHOOK_VAULT_URL and WEBHOOK_SIGNER_PRINCIPAL are required when outbound webhooks are enabled")
	}
	if production && c.AllowHTTP {
		return c, errors.New("WEBHOOK_ALLOW_HTTP must not be set in production: webhooks go over https only")
	}
	return c, nil
}
