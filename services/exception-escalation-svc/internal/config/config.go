package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Port                     string
	DatabaseURL              string
	KafkaBrokers             string
	KafkaEventsTopic         string
	AuthzServiceURL          string
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
	JurisdictionRulesURL     string

	// EventResidencyRegion (EVENT_RESIDENCY_REGION, e.g. uk or eu-west) is stamped on every
	// event written to the shared eventing outbox (the BIZ-08 deadline events). Required.
	EventResidencyRegion string
	// Env (ENV) is "local" for developer stacks; only then may the Kafka writer auto-create the topic.
	Env string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8133"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	region := strings.TrimSpace(os.Getenv("EVENT_RESIDENCY_REGION"))
	if region == "" {
		return nil, fmt.Errorf("EVENT_RESIDENCY_REGION is required (the residency region stamped on every shared-outbox event)")
	}
	return &Config{
		EventResidencyRegion: region,
		Env:                  getEnvOrDefault("ENV", "local"),
		Port:                     port,
		DatabaseURL:              dbURL,
		KafkaBrokers:             getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:         getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.exception-escalation.events"),
		AuthzServiceURL:          getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		JurisdictionRulesURL:     getEnvOrDefault("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8081"),
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
