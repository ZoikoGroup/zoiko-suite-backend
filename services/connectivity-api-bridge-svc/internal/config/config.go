package config

import (
	"os"
)

type Config struct {
	Port             string
	DatabaseURL      string
	KafkaBrokers     string
	KafkaEventsTopic string
	AuthzServiceURL  string
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8144"),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/zoiko_connectivity_api_bridge?sslmode=disable"),
		KafkaBrokers:     getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic: getEnv("KAFKA_EVENTS_TOPIC", "zoiko.connectivity.api.bridge.events"),
		AuthzServiceURL:  getEnv("AUTHZ_SERVICE_URL", "http://localhost:8081"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
