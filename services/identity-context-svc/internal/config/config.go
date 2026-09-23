// Package config provides typed, env-driven configuration for identity-context-svc.
package config

import (
	"errors"
	"fmt"
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
	TenantRegistryURL string
	AccessControlURL  string

	// Authorization Service URL for admin mutation authorization checks.
	// Must be set in production/staging; a placeholder is allowed only in local development.
	AuthzServiceURL string

	// AuthzEnv is the tier authz.NewClient applies its production guard
	// against. It DEFAULTS TO Environment and may never downgrade it — see
	// the derivation in Load, which is the whole reason this field is not
	// simply read from its own variable.
	AuthzEnv string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// SIEMServiceURL is siem-integration-svc. Empty disables streaming —
	// see internal/siem's doc comment.
	SIEMServiceURL string

	// ── GOV-01 completion ────────────────────────────────────────────────────

	// Environment is the deployment tier stamped on every
	// TenantContextDecision. One of local/development/staging/production; the
	// schema CHECKs the same four, so an invalid value here would surface as a
	// constraint violation on the first resolution rather than at boot.
	Environment string

	// DeploymentRegion is where this instance runs, compared against each
	// entity's data residency policy.
	DeploymentRegion string

	// AllowedResidencyPolicies is the set of data_residency_policy_id values
	// this region may serve. EMPTY DISABLES ENFORCEMENT, which is the correct
	// default for a control being introduced into a running estate: refusing
	// every resolution until somebody populates a list would take the platform
	// down, and a control that does that on deployment gets reverted rather
	// than fixed.
	AllowedResidencyPolicies []string

	// IngressPolicy is "observe" (default) or "strict". Observe records the
	// ingress and refuses a MISMATCH but permits an unbound identifier; strict
	// refuses an unbound one too. See context.IngressPolicy.
	IngressPolicy string

	// SoDServiceURL is GOV-04. Mandatory in staging/production, where
	// sod.NewChecker refuses to build a stub.
	SoDServiceURL string

	// SupportContextMaxTTLSeconds bounds a break-glass elevation. The spec's
	// invariant is "time-limited"; without a ceiling that means "expires
	// eventually", which a caller can set to a decade.
	SupportContextMaxTTLSeconds     int
	SupportContextDefaultTTLSeconds int

	// SessionEvidenceRetentionDays is how long session evidence is kept before
	// disposition. Zero leaves disposition_due_at NULL, which the sweep reads
	// as "never due" — the safe default, because a misconfigured period should
	// keep evidence too long rather than delete it early.
	SessionEvidenceRetentionDays int

	// RetentionSweepIntervalMinutes is how often the disposition sweep runs.
	RetentionSweepIntervalMinutes int

	// OutboxRetentionDays is how long DELIVERED outbox rows are kept before
	// being purged. They carry no evidential weight of their own.
	OutboxRetentionDays int

	// OutboxRelayBatchSize and OutboxRelayPollMillis tune the outbox drain.
	OutboxRelayBatchSize  int
	OutboxRelayPollMillis int

	// SupportReviewIntervalMinutes is how often expired-but-unreviewed support
	// contexts are reported. Zero disables the reconciler, which should only
	// ever be done deliberately: an unreviewed break-glass is the control's
	// most common silent failure.
	SupportReviewIntervalMinutes int
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
	// URL is a complete connection string and, when non-empty, is the ONLY
	// field read — Host, Port, Password and TLSEnabled are ignored. Managed
	// Redis (Upstash, Redis Cloud, ElastiCache Serverless) hands out exactly
	// one string:
	//
	//   rediss://default:<token>@<name>-<id>.upstash.io:6379
	//
	// The scheme is `rediss`, with two s. Plain `redis://` leaves TLS off and a
	// managed endpoint drops the connection rather than speaking cleartext,
	// which arrives as an i/o timeout on the startup Ping — not as the
	// authentication error the missing TLS actually is.
	URL string

	Host string
	Port int
	// Password is empty for the local container, which runs without
	// requirepass. Every managed endpoint requires it.
	Password string
	// TLSEnabled wraps a Host/Port connection in TLS. Ignored when URL is set,
	// where the scheme decides instead.
	TLSEnabled bool

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
			URL:                   env("REDIS_URL", ""),
			Host:                  env("REDIS_HOST", "localhost"),
			Port:                  envInt("REDIS_PORT", 6379),
			Password:              env("REDIS_PASSWORD", ""),
			TLSEnabled:            envBool("REDIS_TLS_ENABLED", false),
			SessionTTLSeconds:     envInt("SESSION_CACHE_TTL_SECONDS", 300),
			RoleProfileTTLSeconds: envInt("ROLE_PROFILE_CACHE_TTL_SECONDS", 900),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "identity-context-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.identity.events"),
		},
		TenantRegistryURL:    env("TENANT_REGISTRY_URL", "http://tenant-registry-svc"),
		AccessControlURL:     env("ACCESS_CONTROL_URL", "http://access-control-svc"),
		AuthzServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthzEnv:             env("AUTHZ_ENV", ""), // derived below; never defaulted here
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		SIEMServiceURL:       env("SIEM_SERVICE_URL", ""),

		Environment:              env("DEPLOY_ENVIRONMENT", "local"),
		DeploymentRegion:         env("DEPLOYMENT_REGION", "local"),
		AllowedResidencyPolicies: envList("ALLOWED_RESIDENCY_POLICIES"),
		IngressPolicy:            env("INGRESS_POLICY", "observe"),
		SoDServiceURL:            env("SOD_SERVICE_URL", ""),

		SupportContextMaxTTLSeconds:     envInt("SUPPORT_CONTEXT_MAX_TTL_SECONDS", 4*60*60),
		SupportContextDefaultTTLSeconds: envInt("SUPPORT_CONTEXT_DEFAULT_TTL_SECONDS", 60*60),

		SessionEvidenceRetentionDays:  envInt("SESSION_EVIDENCE_RETENTION_DAYS", 0),
		RetentionSweepIntervalMinutes: envInt("RETENTION_SWEEP_INTERVAL_MINUTES", 360),
		OutboxRetentionDays:           envInt("OUTBOX_RETENTION_DAYS", 7),
		OutboxRelayBatchSize:          envInt("OUTBOX_RELAY_BATCH_SIZE", 100),
		OutboxRelayPollMillis:         envInt("OUTBOX_RELAY_POLL_MILLIS", 1000),
		SupportReviewIntervalMinutes:  envInt("SUPPORT_REVIEW_INTERVAL_MINUTES", 60),
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

	// The environment is written into a column with a CHECK constraint on the
	// same four values. Validating here turns what would be a constraint
	// violation on the first resolution — after the service is live and
	// serving — into a refusal to start.
	switch cfg.Environment {
	case "local", "development", "staging", "production":
	default:
		return nil, fmt.Errorf(
			"DEPLOY_ENVIRONMENT %q is invalid: expected local, development, staging or production",
			cfg.Environment)
	}

	// AUTHZ_ENV DEFAULTS TO THE DEPLOYMENT ENVIRONMENT AND MAY NOT DOWNGRADE IT.
	//
	// It used to default to "development" independently of
	// DEPLOY_ENVIRONMENT, and nothing in the estate ever set it — not the
	// compose file, not the Kubernetes manifests, not the runbook. The effect
	// was that authz.NewClient's production guard could not fire in ANY
	// deployment: a production service that never set AUTHZ_SERVICE_URL took
	// the default placeholder, was told it was in development, and built the
	// PERMIT-ALL STUB, announcing it in a warning log nobody reads.
	//
	// A second environment variable naming the same fact is a second chance to
	// get it wrong, and one nobody was taking. The tier is now derived from the
	// tier, and an explicit AUTHZ_ENV may only agree with it.
	if cfg.AuthzEnv == "" {
		cfg.AuthzEnv = cfg.Environment
	}
	if cfg.Environment == "production" || cfg.Environment == "staging" {
		if !strings.EqualFold(cfg.AuthzEnv, cfg.Environment) {
			return nil, fmt.Errorf(
				"AUTHZ_ENV %q cannot downgrade DEPLOY_ENVIRONMENT %q: it would disable the authorization guard that refuses a placeholder authorization-svc",
				cfg.AuthzEnv, cfg.Environment)
		}
	}

	switch cfg.IngressPolicy {
	case "observe", "strict":
	default:
		return nil, fmt.Errorf("INGRESS_POLICY %q is invalid: expected observe or strict", cfg.IngressPolicy)
	}

	// A support window with no ceiling is a standing back door, so this is a
	// hard floor rather than a default that can be configured away.
	if cfg.SupportContextMaxTTLSeconds < 60 {
		return nil, errors.New("SUPPORT_CONTEXT_MAX_TTL_SECONDS must be at least 60")
	}
	if cfg.SupportContextDefaultTTLSeconds > cfg.SupportContextMaxTTLSeconds {
		return nil, errors.New("SUPPORT_CONTEXT_DEFAULT_TTL_SECONDS cannot exceed SUPPORT_CONTEXT_MAX_TTL_SECONDS")
	}

	// Staging and production must have a real GOV-04. sod.NewChecker refuses
	// to build a stub there, but it does so at wiring time and this message is
	// the one an operator can act on.
	if cfg.Environment == "production" || cfg.Environment == "staging" {
		if cfg.SoDServiceURL == "" {
			return nil, fmt.Errorf(
				"SOD_SERVICE_URL is required in %s: without it segregation of duties is not enforced on privileged commands",
				cfg.Environment)
		}
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

// envList reads a comma-separated variable into a slice, dropping empties.
//
// An unset variable and one set to "" both yield nil rather than []string{""},
// which matters: the residency check treats an empty list as "enforcement off"
// and a list containing one empty string would enforce against a policy id
// nothing can ever match, refusing every resolution.
func envList(key string) []string {
	raw := os.Getenv(key)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
