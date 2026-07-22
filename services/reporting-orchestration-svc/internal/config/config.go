package config

import "os"

type Config struct {
	Port         string
	DatabaseURL  string
	KafkaBrokers string
	KafkaTopic   string
	AuthzURL     string
	LogLevel     string
}

func Load() *Config {
	return &Config{
		Port:         getEnv("PORT", "8138"),
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/reporting_orchestration?sslmode=disable"),
		KafkaBrokers: getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaTopic:   getEnv("KAFKA_TOPIC", "zoiko.reporting-orchestration.events"),
		AuthzURL:     getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089"),
		LogLevel:     getEnv("LOG_LEVEL", "info"),
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
