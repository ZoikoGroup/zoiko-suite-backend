package config

import (
	"os"
)

type Config struct {
	Port                 string
	DatabaseURL          string
	KafkaBrokers         string
	KafkaEventsTopic     string
	AuthzServiceURL      string
	JurisdictionRulesURL string
}

func Load() *Config {
	return &Config{
		Port:                 getEnv("PORT", "8134"),
		DatabaseURL:          getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/anomaly_detection?sslmode=disable"),
		KafkaBrokers:         getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic:     getEnv("KAFKA_EVENTS_TOPIC", "zoiko.anomaly-detection.events"),
		AuthzServiceURL:      getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089"),
		JurisdictionRulesURL: getEnv("JURISDICTION_RULES_URL", "http://localhost:8125"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
