package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                string
	DatabaseURL         string
	KafkaBrokers        string
	KafkaEventsTopic    string
	AuthzServiceURL     string
	WebhookSharedSecret string
	// EventResidencyRegion is the authoritative regional data-bearing
	// context (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion) stamped on
	// every event this service emits through the transactional outbox. No
	// safe default exists; left empty here, envelope.New refuses to build
	// an event without it.
	EventResidencyRegion string

	// Provider callbacks (ZS-SVC-M-001 INT-07). ProviderCallbacksJSON lists the providers this deployment
	// accepts callbacks from and how each is verified and read; secrets come from the vault, per tenant, for
	// the one verification. Empty means the profile-driven endpoint is off.
	ProviderCallbacksJSON string
	VaultURL              string
	VaultWorkloadID       string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8163"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	secret := os.Getenv("WEBHOOK_SHARED_SECRET")
	// No development default: a shared secret that is publicly known is
	// equivalent to no authentication at all. Empty leaves the legacy webhook
	// receiver switched off (it fails closed with 503) while the profile-driven
	// endpoint keeps using per-tenant vault secrets.
	return &Config{
		Port:                  port,
		DatabaseURL:           dbURL,
		KafkaBrokers:          getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:      getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-status.events"),
		AuthzServiceURL:       getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		WebhookSharedSecret:   secret,
		EventResidencyRegion:  getEnvOrDefault("EVENT_RESIDENCY_REGION", ""),
		ProviderCallbacksJSON: os.Getenv("PROVIDER_CALLBACKS_JSON"),
		VaultURL:              getEnvOrDefault("VAULT_SERVICE_URL", "http://secret-vault-integration-svc:8087"),
		VaultWorkloadID:       getEnvOrDefault("VAULT_WORKLOAD_ID", "payment-status-svc"),
	}, nil
}

func (c *Config) DSN() string {
	return c.DatabaseURL
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
