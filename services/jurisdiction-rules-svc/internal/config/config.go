package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for jurisdiction-rules-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of the Authorization Service.
	// Every admin mutating API call must be authorized before proceeding.
	// No service self-authorizes (doctrine).
	AuthZServiceURL string

	// AuthZPlatformScopeID is the legal_entity_id this service presents to
	// authorization-svc when asking for a decision.
	//
	// Jurisdiction data is platform-wide reference data — it has no tenant_id
	// and no owning legal entity (see 000001_initial_schema.up.sql's header).
	// authorization-svc nevertheless rejects an empty legal_entity_id with
	// 400, so every jurisdiction mutation is evaluated against one synthetic
	// platform-scope entity. Role assignments granting JURISDICTION_* actions
	// must be made against this same ID.
	AuthZPlatformScopeID string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string
}

// KafkaConfig holds event-backbone connection parameters. Per
// docs/architecture/03-microservices.md §8.2, this service publishes
// jurisdiction.rule.updated and jurisdiction.rule.activated — see
// internal/events/publisher.go for what is and isn't wired.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// DBConfig holds PostgreSQL connection parameters.
type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

func (d DBConfig) DSN() string {
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

// Load reads configuration from environment variables and validates the
// combinations that must not be allowed to start.
func Load() (*Config, error) {
	cfg := &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8082),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "jurisdiction_rules"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: envList("KAFKA_BROKERS", []string{"localhost:9092"}),
			GroupID: env("KAFKA_GROUP_ID", "jurisdiction-rules-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.jurisdiction.events"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthZPlatformScopeID: env("AUTHZ_PLATFORM_SCOPE_ID", ""),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate rejects configurations that would start the service in an unsafe
// or silently-broken state. Load previously returned a nil error
// unconditionally, so a production deployment with sslmode=disable or no
// authz scope came up looking healthy.
func (c *Config) validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535, got %d", c.Port)
	}

	isProdOrStaging := strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "staging")
	if !isProdOrStaging {
		return nil
	}

	if c.DB.Password == "" {
		return fmt.Errorf("DB_PASSWORD must be set in %s environment", c.Env)
	}
	if strings.EqualFold(c.DB.SSLMode, "disable") {
		return fmt.Errorf("DB_SSLMODE=disable is not permitted in %s environment", c.Env)
	}
	// Without a scope ID every authorize call sends an empty legal_entity_id,
	// which authorization-svc answers 400 — i.e. a fail-closed 503 on every
	// admin mutation. Better to refuse to start than to look healthy.
	if c.AuthZPlatformScopeID == "" {
		return fmt.Errorf("AUTHZ_PLATFORM_SCOPE_ID must be set in %s environment", c.Env)
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envList reads a comma-separated list, trimming blanks.
//
// Unlike env(), an explicitly empty value is honoured rather than replaced by
// the default: `KAFKA_BROKERS=` is how a single-service local run says "no
// event backbone, drop the events". Routing that through env() put the
// default broker back, so the service spent every mutation trying to reach a
// broker that was never going to be there.
func envList(key string, def []string) []string {
	raw, set := os.LookupEnv(key)
	if !set {
		return def
	}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
