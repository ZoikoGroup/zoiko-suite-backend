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

	// AuthzMTLSEnabled turns on the mTLS pilot for calls to authorization-svc.
	// OFF by default — when false, nothing about the existing authz call
	// changes.
	AuthzMTLSEnabled bool
	// AuthzMTLSURL is authorization-svc's mTLS listener, used only when
	// AuthzMTLSEnabled is true.
	AuthzMTLSURL string
	// MTLSManagementServiceURL is where this service provisions its own
	// client-side mTLS identity, used only when AuthzMTLSEnabled is true.
	MTLSManagementServiceURL string
}

func Load() *Config {
	port := getEnv("PORT", "8135")
	dbURL := getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/forecasting?sslmode=disable")
	kafkaBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "zoiko.forecasting.events")
	authzURL := getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089")
	logLevel := getEnv("LOG_LEVEL", "info")
	authzMTLSEnabled := os.Getenv("AUTHZ_MTLS_ENABLED") == "true"
	authzMTLSURL := getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449")
	mtlsManagementServiceURL := getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140")

	return &Config{
		Port:                     port,
		DatabaseURL:              dbURL,
		KafkaBrokers:             kafkaBrokers,
		KafkaTopic:               kafkaTopic,
		AuthzURL:                 authzURL,
		LogLevel:                 logLevel,
		AuthzMTLSEnabled:         authzMTLSEnabled,
		AuthzMTLSURL:             authzMTLSURL,
		MTLSManagementServiceURL: mtlsManagementServiceURL,
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
