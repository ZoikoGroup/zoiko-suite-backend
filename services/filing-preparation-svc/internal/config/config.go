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
	EvidenceManifestURL  string
	JurisdictionRulesURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8130"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                 port,
		DatabaseURL:          dbURL,
		KafkaBrokers:         getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:     getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.filing-prep.events"),
		AuthzServiceURL:      getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		EvidenceManifestURL:  getEnvOrDefault("EVIDENCE_MANIFEST_URL", "http://evidence-manifest-svc:8090"),
		JurisdictionRulesURL: getEnvOrDefault("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8081"),
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
