package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port             string
	DatabaseURL      string
	KafkaBrokers     string
	KafkaEventsTopic string
	AuthzServiceURL  string

	// EventResidencyRegion is the authoritative regional data-bearing
	// context (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion) stamped on
	// every event this service emits through the transactional outbox. No
	// safe default exists; left empty here, envelope.New refuses to build
	// an event without it, same posture as financial-close-svc/
	// bank-reconciliation-svc's EVENT_RESIDENCY_REGION.
	EventResidencyRegion string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// BNK-02 dependencies — see internal/clients's own package docs.
	VaultServiceURL    string
	TreasuryServiceURL string

	// TokenizationEnforced/TokenizationServiceURL turn on tokenization of
	// bank account numbers at write time (Security Standard §14, S3
	// Restricted; internal/tokenization). Disabled by default, like
	// AuthzMTLSEnabled above: while false, account numbers are stored exactly
	// as they are today — a documented known gap, not a fix. It stays false
	// until key-management-svc implements the tokenize/detokenize endpoints
	// internal/tokenization proposes; it is a migration state with an end,
	// not a permanent toggle (SPEC_DEVIATIONS).
	TokenizationEnforced   bool
	TokenizationServiceURL string

	tokenizationEnforcedRaw string
}

// Validate reports configuration the service must not start with.
//
// TOKENIZATION_ENFORCED is parsed strictly. AUTHZ_MTLS_ENABLED-style
// parsing (== "true") would read a typo such as "True" or "yes" as false and
// silently store raw account numbers on a deployment that asked for
// tokenization; for a protection flag that is fail-open, so anything other
// than "true", "false" or unset refuses to start.
func (c *Config) Validate() error {
	switch c.tokenizationEnforcedRaw {
	case "", "false", "true":
	default:
		return fmt.Errorf("TOKENIZATION_ENFORCED must be \"true\" or \"false\", got %q", c.tokenizationEnforcedRaw)
	}
	if c.TokenizationEnforced && c.TokenizationServiceURL == "" {
		return fmt.Errorf("TOKENIZATION_ENFORCED is true but TOKENIZATION_SERVICE_URL is empty")
	}
	return nil
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8145"),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/zoiko_banking_connector?sslmode=disable"),
		KafkaBrokers:     getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic: getEnv("KAFKA_EVENTS_TOPIC", "zoiko.banking.connector.events"),
		AuthzServiceURL:  getEnv("AUTHZ_SERVICE_URL", "http://localhost:8081"),

		EventResidencyRegion: getEnv("EVENT_RESIDENCY_REGION", ""),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		VaultServiceURL:    getEnv("VAULT_SERVICE_URL", "http://secret-vault-integration-svc:8087"),
		TreasuryServiceURL: getEnv("TREASURY_SERVICE_URL", "http://treasury-svc:8103"),

		TokenizationEnforced:    os.Getenv("TOKENIZATION_ENFORCED") == "true",
		TokenizationServiceURL:  getEnv("TOKENIZATION_SERVICE_URL", "http://key-management-svc:8143"),
		tokenizationEnforcedRaw: os.Getenv("TOKENIZATION_ENFORCED"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
