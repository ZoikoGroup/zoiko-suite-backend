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
	// TreasuryServiceURL is the base URL of treasury-svc, used by
	// PrepareAttempt to verify PayerAccountRef for real (BNK-01's
	// account_status + is_ownership_verified) instead of trusting the
	// caller-supplied PayerAccountVerified flag alone.
	TreasuryServiceURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8162"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:             port,
		DatabaseURL:      dbURL,
		KafkaBrokers:     getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic: getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-initiation.events"),
		AuthzServiceURL:    getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		TreasuryServiceURL: getEnvOrDefault("TREASURY_SERVICE_URL", "http://treasury-svc:8103"),
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
