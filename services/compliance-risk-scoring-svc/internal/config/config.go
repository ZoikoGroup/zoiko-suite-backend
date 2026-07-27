package config

import (
	"os"
)

type Config struct {
	Port         string
	DatabaseURL  string
	KafkaBrokers string
	KafkaTopic   string
	AuthzURL     string
	LogLevel     string
}

func Load() *Config {
	port := getEnv("PORT", "8136")
	dbURL := getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/compliance_risk_scoring?sslmode=disable")
	kafkaBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "zoiko.compliance-risk-scoring.events")
	authzURL := getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089")
	logLevel := getEnv("LOG_LEVEL", "info")

	return &Config{
		Port:         port,
		DatabaseURL:  dbURL,
		KafkaBrokers: kafkaBrokers,
		KafkaTopic:   kafkaTopic,
		AuthzURL:     authzURL,
		LogLevel:     logLevel,
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
