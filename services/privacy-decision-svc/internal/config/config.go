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
	PurposeRegistryURL   string
	ConsentRegistryURL   string
	RetentionRegistryURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8153"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                 port,
		DatabaseURL:          dbURL,
		KafkaBrokers:         getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:     getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.privacy-decision.events"),
		PurposeRegistryURL:   getEnvOrDefault("PURPOSE_REGISTRY_URL", "http://privacy-purpose-registry-svc:8151"),
		ConsentRegistryURL:   getEnvOrDefault("CONSENT_REGISTRY_URL", "http://privacy-consent-svc:8152"),
		RetentionRegistryURL: getEnvOrDefault("RETENTION_REGISTRY_URL", "http://retention-registry-svc:8148"),
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
