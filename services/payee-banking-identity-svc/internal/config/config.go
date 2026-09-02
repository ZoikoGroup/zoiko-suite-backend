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
	return &Config{
		Port:                   port,
		DatabaseURL:            dbURL,
		KafkaBrokers:           getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:       getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payee-banking-identity.events"),
		AuthzServiceURL:        getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		CounterpartyServiceURL: getEnvOrDefault("COUNTERPARTY_SERVICE_URL", "http://counterparty-management-svc:8124"),
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
