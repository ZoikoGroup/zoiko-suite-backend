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

	// FinancialControlURL is financial-control-svc's base URL — the
	// governed, versioned TolerancePolicy registry PerformIntelligentReconciliation
	// fetches its write-off tolerances from (ZS-SVC-Z-001 INV-09).
	FinancialControlURL string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzURL (plain HTTP) keeps being used unless
	// explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() *Config {
	port := getEnv("PORT", "8137")
	dbURL := getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/reconciliation_intelligence?sslmode=disable")
	kafkaBrokers := getEnv("KAFKA_BROKERS", "localhost:9092")
	kafkaTopic := getEnv("KAFKA_TOPIC", "zoiko.reconciliation-intelligence.events")
	authzURL := getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089")
	logLevel := getEnv("LOG_LEVEL", "info")
	financialControlURL := getEnv("FINANCIAL_CONTROL_SERVICE_URL", "http://financial-control-svc:8171")

	return &Config{
		Port:                port,
		DatabaseURL:         dbURL,
		KafkaBrokers:        kafkaBrokers,
		KafkaTopic:          kafkaTopic,
		AuthzURL:            authzURL,
		LogLevel:            logLevel,
		FinancialControlURL: financialControlURL,

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
