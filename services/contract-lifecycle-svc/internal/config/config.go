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
	AuthzServiceURL      string
	JurisdictionRulesURL string
	GovernanceLogURL     string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// GovernanceLogMTLSURL, when set, is governance-decision-log-svc's mTLS
	// listener (https://governance-decision-log-svc:8450). Decision reads then
	// present this service's workload certificate, provisioned with its own
	// bootstrap token (MTLS_BOOTSTRAP_TOKEN_PATH), and read as
	// GovernanceLogPrincipalID (D2-2.F07). Empty keeps the plain
	// GovernanceLogURL, which a decision log in IDENTITY_VERIFY=require refuses.
	GovernanceLogMTLSURL     string
	GovernanceLogPrincipalID string
	MTLSBootstrapTokenPath   string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8119"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                 port,
		DatabaseURL:          dbURL,
		KafkaBrokers:         getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:     getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.contract.events"),
		AuthzServiceURL:      getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		JurisdictionRulesURL: getEnvOrDefault("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8081"),
		GovernanceLogURL:     getEnvOrDefault("GOVERNANCE_LOG_URL", "http://governance-svc:8083"),

		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		GovernanceLogMTLSURL:     os.Getenv("GOVERNANCE_LOG_MTLS_URL"),
		GovernanceLogPrincipalID: getEnvOrDefault("GOVERNANCE_LOG_PRINCIPAL_ID", "svc-contract-lifecycle"),
		MTLSBootstrapTokenPath:   os.Getenv("MTLS_BOOTSTRAP_TOKEN_PATH"),
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
