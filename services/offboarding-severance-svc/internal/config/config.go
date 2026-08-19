package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                 string
	DBHost               string
	DBPort               string
	DBName               string
	DBUser               string
	DBPassword           string
	DBSslMode            string
	KafkaBrokers         string
	KafkaEventsTopic     string
	EmployeeMasterURL    string
	JurisdictionRulesURL string
	AuthzServiceURL      string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:                 getEnv("PORT", "8117"),
		DBHost:               getEnv("DB_HOST", "localhost"),
		DBPort:               getEnv("DB_PORT", "5432"),
		DBName:               getEnv("DB_NAME", "offboarding_severance"),
		DBUser:               getEnv("DB_USER", "postgres"),
		DBPassword:           getEnv("DB_PASSWORD", "postgres"),
		DBSslMode:            getEnv("DB_SSLMODE", "disable"),
		KafkaBrokers:         getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic:     getEnv("KAFKA_EVENTS_TOPIC", "zoiko.offboarding.events"),
		EmployeeMasterURL:    getEnv("EMPLOYEE_MASTER_URL", "http://localhost:8108"),
		JurisdictionRulesURL: getEnv("JURISDICTION_RULES_URL", "http://localhost:8081"),
		AuthzServiceURL:      getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089"),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}

	return cfg, nil
}

func (c *Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSslMode,
	)
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
