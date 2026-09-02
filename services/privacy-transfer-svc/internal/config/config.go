package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port               string
	DatabaseURL        string
	KafkaBrokers       string
	KafkaEventsTopic   string
	AuthzServiceURL    string
	PurposeRegistryURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8155"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:               port,
		DatabaseURL:        dbURL,
		KafkaBrokers:       getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:   getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.privacy-transfer.events"),
		AuthzServiceURL:    getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		PurposeRegistryURL: getEnvOrDefault("PURPOSE_REGISTRY_URL", "http://privacy-purpose-registry-svc:8151"),
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
