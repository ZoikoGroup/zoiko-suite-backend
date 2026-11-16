package config

import (
	"fmt"
	"os"
	"regexp"
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

	// EventResidencyRegion is the authoritative regional data-bearing
	// context (ZS-EVENT-001 §4, envelope.Spec.ResidencyRegion) stamped on
	// every emitted event. Required, with no default: the envelope rejects
	// an empty region, so a missing value would fail every write at request
	// time. Refusing to start surfaces it at deploy time instead, and the
	// code never invents a region nobody configured.
	EventResidencyRegion string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8145"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if !residencyRegion.MatchString(region) {
		return nil, fmt.Errorf("EVENT_RESIDENCY_REGION is required and must be a lower-case region code (e.g. \"uk\", \"eu-west\"), got %q", region)
	}
	return &Config{
		EventResidencyRegion: region,
		Port:                 port,
		DatabaseURL:          dbURL,
		KafkaBrokers:         getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:     getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.capability-registry.events"),
		AuthzServiceURL:      getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}, nil
}

// residencyRegion mirrors the envelope package's own check on
// Spec.ResidencyRegion, so a bad value is refused at startup rather than on
// the first write.
var residencyRegion = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (c *Config) DSN() string {
	return c.DatabaseURL
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
