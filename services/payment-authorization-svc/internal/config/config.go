package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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
	// AuthorizationTTL is how long an unused authorization stays valid.
	AuthorizationTTL time.Duration
	// ExpirySweepInterval is how often overdue authorizations are expired.
	ExpirySweepInterval time.Duration
	// HighValueSignatures is how many distinct signers a high-value payment needs.
	HighValueSignatures int
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
		AuthorizationTTL:          getDurationOrDefault("AUTHORIZATION_TTL", 24*time.Hour),
		ExpirySweepInterval:       getDurationOrDefault("EXPIRY_SWEEP_INTERVAL", time.Minute),
		HighValueSignatures:       getIntOrDefault("HIGH_VALUE_REQUIRED_SIGNATURES", 2),
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

func getDurationOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}

func getIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	return def
}
