package config

import (
	"fmt"
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
	// MTLSBootstrapTokenPath points at the shared self-provisioning secret
	// (see deployments/docker-compose.yml's mtls-bootstrap-keygen). Empty
	// means no bootstrap token is presented, which is fine when
	// AuthzMTLSEnabled is false, but will fail provisioning if enabled
	// without also setting this.
	MTLSBootstrapTokenPath string

	KillSwitchRegistryServiceURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8146"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:             port,
		DatabaseURL:      dbURL,
		KafkaBrokers:     getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic: getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.ai-governance.events"),
		AuthzServiceURL:  getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		MTLSBootstrapTokenPath:   getEnvOrDefault("MTLS_BOOTSTRAP_TOKEN_PATH", ""),

		KillSwitchRegistryServiceURL: getEnvOrDefault("KILL_SWITCH_REGISTRY_SERVICE_URL", "http://kill-switch-registry-svc:8147"),
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
