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

	// Schema is the Postgres schema holding this service's tables, applied to
	// the connection as search_path.
	//
	// Empty means the server default, which is what a database-per-service
	// deployment uses: the service owns a whole database and its tables sit in
	// that database's public schema.
	//
	// A managed single-database host cannot express the 63 databases
	// deployments/init-db.sh creates, so each service gets a schema instead. The
	// migrations need no change -- they say CREATE TABLE, which lands wherever
	// search_path points.
	//
	// A schema named here that does not exist is NOT a connect-time error:
	// Postgres drops unresolvable entries from search_path silently, so the
	// failure surfaces on the first query as "relation ... does not exist".
	// Check the schema exists and that DB_USER holds USAGE on it.
	Schema string

	// Options is appended to the DSN verbatim, for pgx settings that vary by
	// deployment rather than by service.
	//
	// The case this exists for is connection pooling. pgx defaults to cached
	// NAMED prepared statements, which PgBouncer in transaction mode breaks: the
	// statement is prepared on one server connection and executed on another.
	// The error surfaces only under concurrency, so it passes every smoke test
	// and then fails in production.
	//
	//	DB_OPTIONS="default_query_exec_mode=exec statement_cache_capacity=0"
	//
	// Leave empty when connecting to a database directly.
	Options string
}

func (d DBConfig) DSN() string {
	dsn := "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + quoteDSNValue(d.Password) +
		" sslmode=" + d.SSLMode
	if d.Schema != "" {
		dsn += " search_path=" + d.Schema
	}
	if d.Options != "" {
		dsn += " " + d.Options
	}
	return dsn
}

// quoteDSNValue renders v as a single-quoted keyword/value DSN literal.
//
// The password used to be interpolated bare, which silently produces a
// MALFORMED DSN for any password containing a space, a single quote or a
// backslash -- and managed database providers generate exactly those. The
// resulting connection error names whichever parameter the stray character
// happened to split, so it reads as a typo in an unrelated field rather than
// as a quoting problem.
//
// libpq's rule for a quoted value is that a backslash and a single quote are
// each escaped with a backslash. Written as an explicit loop rather than a
// strings.NewReplacer because the replacer's arguments would be raw string
// literals whose contents are themselves backslashes, where correct-by-one-level
// and wrong-by-two look identical at a glance.
func quoteDSNValue(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(v); i++ {
		if c := v[i]; c == '\'' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('\'')
	return b.String()
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
			// Empty keeps the server default schema, which is what the
			// database-per-service local stack uses. Set to this service's
			// schema name when the database is a managed single-database host.
			Schema: env("DB_SCHEMA", ""),
			// pgx tuning that varies by deployment, not by service. Required
			// when connecting through a transaction-mode connection pooler:
			// "default_query_exec_mode=exec statement_cache_capacity=0".
			Options: env("DB_OPTIONS", ""),
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
