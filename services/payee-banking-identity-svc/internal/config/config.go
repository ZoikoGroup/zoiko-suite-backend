package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                   string
	DatabaseURL            string
	KafkaBrokers           string
	KafkaEventsTopic       string
	AuthzServiceURL        string
	CounterpartyServiceURL string
	// EventResidencyRegion is the authoritative regional data-bearing
	// context (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion) stamped on
	// every event this service emits through the transactional outbox. No
	// safe default exists; left empty here, envelope.New refuses to build
	// an event without it.
	EventResidencyRegion string

	// TokenizationEnforced/TokenizationServiceURL turn on tokenization of
	// payee account identifiers at write time (Security Standard §14, S3
	// Restricted; internal/tokenization). Disabled by default: while false,
	// identifiers are stored exactly as they are today — the documented known
	// gap in internal/domain's package doc, not a fix. It stays false until
	// key-management-svc implements the tokenize/detokenize endpoints
	// internal/tokenization proposes; it is a migration state with an end,
	// not a permanent toggle (SPEC_DEVIATIONS).
	TokenizationEnforced   bool
	TokenizationServiceURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8166"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	// Parsed strictly: an `== "true"` check would read a typo such as "True"
	// as false and silently store raw identifiers on a deployment that asked
	// for tokenization — fail-open, for a protection flag.
	enforced := os.Getenv("TOKENIZATION_ENFORCED")
	switch enforced {
	case "", "false", "true":
	default:
		return nil, fmt.Errorf("TOKENIZATION_ENFORCED must be \"true\" or \"false\", got %q", enforced)
	}
	tokenizationURL := getEnvOrDefault("TOKENIZATION_SERVICE_URL", "http://key-management-svc:8143")
	return &Config{
		TokenizationEnforced:   enforced == "true",
		TokenizationServiceURL: tokenizationURL,

		Port:                   port,
		DatabaseURL:            dbURL,
		KafkaBrokers:           getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:       getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payee-banking-identity.events"),
		AuthzServiceURL:        getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		CounterpartyServiceURL: getEnvOrDefault("COUNTERPARTY_SERVICE_URL", "http://counterparty-management-svc:8124"),
		EventResidencyRegion:   getEnvOrDefault("EVENT_RESIDENCY_REGION", ""),
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
