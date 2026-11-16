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

	// EventResidencyRegion and EventClassification are the ZS-EVENT-001 §4
	// envelope attributes stamped on every event. Both are governed,
	// per-deployment decisions, so both are required with no default: the
	// shared envelope rejects an empty or unrecognised value, which would fail
	// every write at request time. Load refuses them at startup instead.
	EventResidencyRegion string
	EventClassification  string
}

// residencyRegion is the envelope library's controlled-code shape.
var residencyRegion = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8144"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if !residencyRegion.MatchString(region) {
		return nil, fmt.Errorf("EVENT_RESIDENCY_REGION is required and must be a lower-case region code (e.g. \"uk\", \"eu-west\"), got %q", region)
	}
	classification := os.Getenv("EVENT_CLASSIFICATION")
	switch classification {
	case "public", "internal", "confidential", "restricted":
	default:
		return nil, fmt.Errorf("EVENT_CLASSIFICATION is required and must be one of public, internal, confidential, restricted, got %q", classification)
	}
	return &Config{
		EventResidencyRegion: region,
		EventClassification:  classification,

		Port:             port,
		DatabaseURL:      dbURL,
		KafkaBrokers:     getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic: getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.commercial-account.events"),
		AuthzServiceURL:  getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
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
