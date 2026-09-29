package config

import "os"

type Config struct {
	Port         string
	DatabaseURL  string
	KafkaBrokers string
	KafkaTopic   string
	AuthzURL     string
	LogLevel     string

	// AUD-10 export/redact/deliver dependencies — see
	// internal/archivestore and internal/retention's own package docs.
	AuditEventStoreURL   string
	RetentionRegistryURL string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzURL (plain HTTP) keeps being used unless
	// explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() *Config {
	return &Config{
		Port:         getEnv("PORT", "8138"),
		DatabaseURL:  getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/reporting_orchestration?sslmode=disable"),
		KafkaBrokers: getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaTopic:   getEnv("KAFKA_TOPIC", "zoiko.reporting-orchestration.events"),
		AuthzURL:     getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089"),
		LogLevel:     getEnv("LOG_LEVEL", "info"),

		AuditEventStoreURL:   getEnv("AUDIT_EVENT_STORE_SERVICE_URL", "http://audit-event-store-svc:8080"),
		RetentionRegistryURL: getEnv("RETENTION_REGISTRY_SERVICE_URL", "http://retention-registry-svc:8148"),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
