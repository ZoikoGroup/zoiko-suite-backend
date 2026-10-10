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
	// PayeeIdentityServiceURL is payee-banking-identity-svc (ORG-10).
	PayeeIdentityServiceURL string
	// AllowServiceReads lets the list/eligibility reads be served without X-Principal-Id
	// (callers payment-proposal-svc / payment-authorization-svc do not forward it yet).
	AllowServiceReads bool
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8156"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                    port,
		DatabaseURL:             dbURL,
		KafkaBrokers:            getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:        getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.supplier-financial-profile.events"),
		AuthzServiceURL:         getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		PayeeIdentityServiceURL: getEnvOrDefault("PAYEE_IDENTITY_SERVICE_URL", "http://payee-banking-identity-svc:8166"),
		AllowServiceReads:       getEnvOrDefault("ALLOW_SERVICE_READS_WITHOUT_PRINCIPAL", "true") != "false",
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
