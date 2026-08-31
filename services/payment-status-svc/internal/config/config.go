package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                string
	DatabaseURL         string
	KafkaBrokers        string
	KafkaEventsTopic    string
	AuthzServiceURL     string
	WebhookSharedSecret string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8163"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	secret := os.Getenv("WEBHOOK_SHARED_SECRET")
	if secret == "" {
		// A real provider integration would set this to the actual secret
		// the provider issues at webhook-configuration time. This default
		// only exists so the service is runnable in a fresh dev
		// environment with no real provider connected — it must never be
		// used in any environment a real callback could reach.
		secret = "dev-only-shared-secret-replace-before-any-real-integration"
	}
	return &Config{
		Port:                port,
		DatabaseURL:         dbURL,
		KafkaBrokers:        getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:    getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-status.events"),
		AuthzServiceURL:     getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		WebhookSharedSecret: secret,
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
