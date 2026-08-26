// Package config provides typed, env-driven configuration for identity-context-svc.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for identity-context-svc.
// All values are sourced from environment variables — no hard-coded secrets.
type Config struct {
	Port int

	// JWT envelope signing (Q2 — signed short-lived JWT)
	// Production: RS256 via KMS-backed keypair through Secret Vault Integration Service.
	JWTSigningPrivateKeyPath string
	JWTKeyID                 string

	// TODO: replace JWTSigningSecret with KMS key reference before Phase 1 production cutover.
	JWTSigningSecret      string
	JWTIssuer             string
	JWTAudienceInternal   string
	EnvelopeJWTTTLSeconds int

	// IdPTokenTTLSeconds is the lifetime of the bearer token POST
	// /v1/authenticate mints. It is an intermediate credential exchanged
	// immediately for an identity envelope, so it is short by default: the
	// window in which a stolen one is useful should be measured against the
	// round trip that redeems it, not against a user's working day.
	IdPTokenTTLSeconds int

	// Password hashing (argon2id). See internal/credential.
	//
	// ArgonMemoryKiB multiplied by ArgonMaxConcurrent is the peak memory the
	// authentication path can hold at once, and this service runs under a
	// 256 MiB container limit. Raising either without checking the other is
	// how a password-guessing attempt becomes an OOM kill of a Tier 0 service.
	ArgonMemoryKiB     int
	ArgonIterations    int
	ArgonParallelism   int
	ArgonMaxConcurrent int

	// Lockout policy for online password guessing.
	AuthMaxFailedAttempts   int
	AuthLockDurationSeconds int

	DB    DBConfig
	Redis RedisConfig
	Kafka KafkaConfig

	// Upstream Tier 0 service base URLs (read-only calls only)
	TenantRegistryURL     string
	DelegatedAuthorityURL string
	AccessControlURL      string

	// Authorization Service URL for admin mutation authorization checks.
	// Must be set in production/staging; a placeholder is allowed only in local development.
	AuthzServiceURL string
	AuthzEnv        string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// SIEMServiceURL is siem-integration-svc. Empty disables streaming —
	// see internal/siem's doc comment.
	SIEMServiceURL string
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

type RedisConfig struct {
	Host string
	Port int
	// SessionTTLSeconds — hot-path cache TTL for signed envelope JWT (default 5 min)
	SessionTTLSeconds int
	// RoleProfileTTLSeconds — role profile cache TTL (default 15 min)
	RoleProfileTTLSeconds int
}

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables with safe defaults.
// Returns an error if any mandatory value is missing or invalid.
func Load() (*Config, error) {
	cfg := &Config{
		Port:                     envInt("PORT", 8080),
		JWTSigningSecret:         env("JWT_SIGNING_SECRET", ""),
		JWTIssuer:                env("JWT_ISSUER", "identity-context-svc"),
		JWTAudienceInternal:      env("JWT_AUDIENCE", "zoiko-internal"),
		JWTSigningPrivateKeyPath: env("JWT_SIGNING_PRIVATE_KEY_PATH", "./envelope_signing_key.pem"),
		JWTKeyID:                 env("JWT_KEY_ID", "envelope-signing-key-1"),

		EnvelopeJWTTTLSeconds: envInt("ENVELOPE_JWT_TTL_SECONDS", 300),
		IdPTokenTTLSeconds:    envInt("IDP_TOKEN_TTL_SECONDS", 300),

		ArgonMemoryKiB:     envInt("ARGON2_MEMORY_KIB", 19456),
		ArgonIterations:    envInt("ARGON2_ITERATIONS", 2),
		ArgonParallelism:   envInt("ARGON2_PARALLELISM", 1),
		ArgonMaxConcurrent: envInt("ARGON2_MAX_CONCURRENT", 4),

		AuthMaxFailedAttempts:   envInt("AUTH_MAX_FAILED_ATTEMPTS", 5),
		AuthLockDurationSeconds: envInt("AUTH_LOCK_DURATION_SECONDS", 900),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "identity_context"),
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
		Redis: RedisConfig{
			Host:                  env("REDIS_HOST", "localhost"),
			Port:                  envInt("REDIS_PORT", 6379),
			SessionTTLSeconds:     envInt("SESSION_CACHE_TTL_SECONDS", 300),
			RoleProfileTTLSeconds: envInt("ROLE_PROFILE_CACHE_TTL_SECONDS", 900),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "identity-context-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.identity.events"),
		},
		TenantRegistryURL:     env("TENANT_REGISTRY_URL", "http://tenant-registry-svc"),
		DelegatedAuthorityURL: env("DELEGATED_AUTHORITY_URL", "http://delegated-authority-svc"),
		AccessControlURL:      env("ACCESS_CONTROL_URL", "http://access-control-svc"),
		AuthzServiceURL:       env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthzEnv:              env("AUTHZ_ENV", "development"),
		OTELExporterEndpoint:  env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		SIEMServiceURL:        env("SIEM_SERVICE_URL", ""),
	}

	// JWT_SIGNING_SECRET is mandatory and must be at least 32 bytes for HS256.
	if cfg.JWTSigningSecret == "" {
		return nil, errors.New("JWT_SIGNING_SECRET is required")
	}
	if len(cfg.JWTSigningSecret) < 32 {
		return nil, errors.New("JWT_SIGNING_SECRET must be at least 32 bytes")
	}

	// Lockout must actually bound guessing. A zero or negative threshold would
	// read as "lock immediately" or "never lock" depending on the comparison,
	// and neither is a setting anyone means to choose.
	if cfg.AuthMaxFailedAttempts < 1 {
		return nil, errors.New("AUTH_MAX_FAILED_ATTEMPTS must be at least 1")
	}
	if cfg.AuthLockDurationSeconds < 1 {
		return nil, errors.New("AUTH_LOCK_DURATION_SECONDS must be at least 1")
	}
	// The argon2id cost factors are validated by credential.Params.Validate at
	// hasher construction; only the concurrency cap is this package's to check,
	// since it is the term that has no meaning inside the algorithm.
	if cfg.ArgonMaxConcurrent < 1 {
		return nil, errors.New("ARGON2_MAX_CONCURRENT must be at least 1")
	}

	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
