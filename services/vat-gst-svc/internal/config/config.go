package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	KafkaBrokers         string
	KafkaEventsTopic     string
	AuthzServiceURL      string
	TaxRulesServiceURL   string
	JurisdictionRulesURL string
	// EvidenceRequirementsURL is the base URL of evidence-requirements-svc.
	// Filing is a finalization path and must not skip required evidence.
	EvidenceRequirementsURL string

	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8127"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                    port,
		DatabaseURL:             dbURL,
		KafkaBrokers:            getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:        getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.vat-gst.events"),
		AuthzServiceURL:         getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		TaxRulesServiceURL:      getEnvOrDefault("TAX_RULES_SERVICE_URL", "http://tax-rules-svc:8125"),
		JurisdictionRulesURL:    getEnvOrDefault("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8081"),
		EvidenceRequirementsURL: getEnvOrDefault("EVIDENCE_REQUIREMENTS_URL", "http://evidence-requirements-svc:8130"),

		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
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
