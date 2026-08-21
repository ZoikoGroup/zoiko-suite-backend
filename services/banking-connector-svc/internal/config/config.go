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

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8145"),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/zoiko_banking_connector?sslmode=disable"),
		KafkaBrokers:     getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic: getEnv("KAFKA_EVENTS_TOPIC", "zoiko.banking.connector.events"),
		AuthzServiceURL:  getEnv("AUTHZ_SERVICE_URL", "http://localhost:8081"),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
