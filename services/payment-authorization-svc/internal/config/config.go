package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                      string
	DatabaseURL               string
	KafkaBrokers              string
	KafkaEventsTopic          string
	AuthzServiceURL           string
	PaymentProposalServiceURL string
	SupplierProfileServiceURL string
	PayeeIdentityServiceURL   string
	PolicyServiceURL          string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8160"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                      port,
		DatabaseURL:               dbURL,
		KafkaBrokers:              getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:          getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-authorization.events"),
		AuthzServiceURL:           getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		PaymentProposalServiceURL: getEnvOrDefault("PAYMENT_PROPOSAL_SERVICE_URL", "http://payment-proposal-svc:8159"),
		SupplierProfileServiceURL: getEnvOrDefault("SUPPLIER_PROFILE_SERVICE_URL", "http://supplier-financial-profile-svc:8156"),
		PayeeIdentityServiceURL:   getEnvOrDefault("PAYEE_IDENTITY_SERVICE_URL", "http://payee-banking-identity-svc:8166"),
		PolicyServiceURL:          getEnvOrDefault("POLICY_SERVICE_URL", "http://policy-svc:8085"),
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
