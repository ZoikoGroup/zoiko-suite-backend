package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for tenant-entity-registry-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is the base URL of the Jurisdiction Rules Service.
	// Used by the JurisdictionValidator client for synchronous fail-closed validation
	// on EntityJurisdictionAssignment creation (Q2 resolution).
	JurisdictionRulesURL string

	// AuthZServiceURL is the base URL of the Authorization Service.
	// Every mutating API call must be authorized before proceeding.
	AuthZServiceURL string

	// AuthZPlatformScopeID is the legal_entity_id presented to
	// authorization-svc for operations that have no tenant yet — in practice
	// only ProvisionTenant, which is what creates the tenant. Every other
	// mutation is scoped to the caller's own tenant. authorization-svc
	// rejects an empty legal_entity_id with 400, so a synthetic
	// platform-scope entity is required; role assignments granting
	// TENANT_PROVISION must be made against this same ID.
	AuthZPlatformScopeID string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string
}

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

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from the environment and validates the
// combinations that must not be allowed to start.
func Load() (*Config, error) {
	cfg := &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8081),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "tenant_entity_registry"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: envList("KAFKA_BROKERS", []string{"localhost:9092"}),
			GroupID: env("KAFKA_GROUP_ID", "tenant-entity-registry-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.entity.events"),
		},
		JurisdictionRulesURL: env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc"),
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthZPlatformScopeID: env("AUTHZ_PLATFORM_SCOPE_ID", ""),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate rejects configurations that would start the service in an unsafe
// or silently-broken state. Load previously returned a nil error
// unconditionally, so a production deployment with sslmode=disable, no
// database password, or no authorization scope came up looking healthy.
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
	// Without a scope ID, ProvisionTenant sends an empty legal_entity_id,
	// which authorization-svc answers 400 — i.e. a fail-closed 503 on every
	// tenant provisioning call. Better to refuse to start than to look healthy.
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
// event backbone". Routing that through env() put the default broker back, so
// the service spent every mutation trying to reach a broker that was never
// going to be there.
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
