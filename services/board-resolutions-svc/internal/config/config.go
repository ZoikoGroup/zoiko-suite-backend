package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	Env              string
	Port             string
	DatabaseURL      string
	KafkaBrokers     []string
	KafkaEventsTopic string
	AuthzServiceURL  string
	EvidenceReqURL   string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8122"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	cfg := &Config{
		Env:              getEnvOrDefault("ENV", "local"),
		Port:             port,
		DatabaseURL:      dbURL,
		KafkaBrokers:     brokers(),
		KafkaEventsTopic: getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.board-resolutions.events"),
		AuthzServiceURL:  getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		EvidenceReqURL:   getEnvOrDefault("EVIDENCE_REQ_URL", "http://evidence-requirements-svc:8130"),

		AuthzMTLSEnabled:         getEnvOrDefault("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnvOrDefault("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnvOrDefault("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}

	// JURISDICTION_RULES_URL used to be loaded here, defaulting to
	// jurisdiction-rules-svc:8081 — a port that belongs to
	// tenant-entity-registry-svc, not to jurisdiction-rules-svc (8082). It was
	// never read by anything, so the wrong number sat in config looking
	// authoritative and would have pointed the first caller at the wrong
	// service. Removed rather than corrected: this service does not talk to
	// jurisdiction-rules-svc.

	// Every field above has a default, so without this an ENV=production
	// deployment that forgot to set them would start anyway and authorize
	// against whatever the defaults named.
	if cfg.Env == "production" {
		var problems []string
		if strings.Contains(cfg.DatabaseURL, "sslmode=disable") {
			problems = append(problems, "DATABASE_URL must not use sslmode=disable in production")
		}
		if strings.Contains(cfg.AuthzServiceURL, "localhost") {
			problems = append(problems, "AUTHZ_SERVICE_URL must be a real authorization-svc address")
		}
		if strings.Contains(cfg.EvidenceReqURL, "localhost") {
			problems = append(problems, "EVIDENCE_REQ_URL must be a real evidence-requirements-svc address")
		}
		if len(problems) > 0 {
			return nil, fmt.Errorf("invalid production config: %s", strings.Join(problems, "; "))
		}
	}

	return cfg, nil
}

func (c *Config) DSN() string {
	return c.DatabaseURL
}

// brokers reads KAFKA_BROKERS with LookupEnv rather than a non-empty check, so
// an explicitly empty value can mean "no broker" — getEnvOrDefault substitutes
// the default for an empty string and made that state unreachable.
func brokers() []string {
	v, ok := os.LookupEnv("KAFKA_BROKERS")
	if !ok {
		return []string{"kafka:9092"}
	}
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
