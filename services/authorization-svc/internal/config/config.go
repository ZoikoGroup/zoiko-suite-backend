package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for authorization-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is used only when creating a jurisdiction-scoped
	// SoD rule — see internal/jurisdiction.HTTPValidator.
	JurisdictionRulesURL string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// MTLS fields wire the material-path mTLS pilot (docs/original_doc/
	// zoiko_suite_doc5.txt:76,251 — mTLS mandated for "material paths", not
	// every call). Disabled by default: MTLSEnabled=false leaves every
	// existing caller on the plain HTTP port untouched. When enabled, this
	// service ALSO listens on MTLSPort with a server certificate obtained
	// from mtls-management-svc, requiring a verified client certificate
	// signed by the same CA — the plain port keeps running alongside it, so
	// turning this on never breaks a caller that hasn't migrated yet.
	MTLSEnabled              bool
	MTLSPort                 int
	MTLSManagementServiceURL string

	// SIEMServiceURL is siem-integration-svc. Empty disables streaming —
	// see internal/siem's doc comment.
	SIEMServiceURL string

	// PlatformScopeEntityID is the synthetic legal_entity_id a platform-wide
	// governance act is authorized against — the same pattern policy-svc uses
	// for a policy with no owning legal entity. Creating a segregation-of-
	// duties rule with no tenant_id applies it to every tenant, so it is
	// authorized against this entity rather than the caller's own tenant
	// scope (handler.requirePlatformAction).
	//
	// Empty by default and empty means REFUSE, not permit: a deployment that
	// has not provisioned the platform-scope entity and its role grant cannot
	// author platform-wide rules at all. That is the safe direction for a
	// control whose blast radius is the whole estate.
	//
	// It is also what handler.PlatformScopeSentinel resolves to, so a caller
	// asking for a platform-wide authorization decision
	// (legal_entity_id=PLATFORM) is evaluated against the same id this
	// service uses for its own platform-wide acts — one id, configured once,
	// instead of each calling service inventing its own synthetic uuid
	// (tracker item 67).
	PlatformScopeEntityID string

	// CacheTTL bounds how long an evaluation read may be served from memory
	// on the /v1/authorize path. See internal/cache's package comment for
	// what is cached (the four evaluation reads plus the ABAC rule lookup)
	// and what deliberately is not (the decision, and the decision-log
	// insert, on every path).
	//
	// This is an in-process cache, so it is also the bound on how long a
	// grant revoked through ANOTHER replica can still authorize. Five seconds
	// by default, deliberately short. AUTHZ_CACHE_TTL_SECONDS=0 disables caching
	// entirely — a real off switch, not a zero-second TTL — for a deployment
	// that cannot accept that window.
	CacheTTL time.Duration

	// AccessDecisionRetentionMonths is how many whole months of decision
	// artifacts the hot access_decision_log partitions keep, for the
	// operational tooling that calls
	// detach_access_decision_log_partitions_before(). 0 means never detach.
	//
	// The service itself never deletes or detaches anything — see migration
	// 000009. This value exists so the retention window is declared next to
	// the rest of the service's configuration rather than living only in
	// whatever cron invokes the function.
	AccessDecisionRetentionMonths int
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

	// DelegationTopic is delegated-authority-svc's event topic, CONSUMED (not
	// produced) so this service can project authority.delegated /
	// authority.revoked / authority.expired into its own
	// delegated_authorities table — see internal/events.Consumer for why
	// exactly these three and not the other events v1 declined to consume.
	//
	// Empty disables consumption entirely, leaving the delegation table fed
	// only by this service's own admin API, which is where it was before.
	// That is the off switch for a deployment where delegated-authority-svc
	// is not running: the consumer would otherwise sit retrying a topic that
	// never appears.
	DelegationTopic string

	// DelegationGroupID is the consumer group for the above. Distinct from
	// GroupID, which names this service as a PRODUCER — sharing one id
	// between a producer and a consumer reads as a single logical
	// subscription and makes offsets impossible to reason about.
	DelegationGroupID string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8089: 8080-8088 are already taken by every other Phase 1 service
		// built so far — see services/README.md.
		Port: envInt("PORT", 8089),
		// DB_NAME defaults to "authorization_svc", not "authorization" —
		// AUTHORIZATION is a reserved SQL keyword (CREATE SCHEMA ...
		// AUTHORIZATION owner), so a bare CREATE DATABASE authorization
		// fails with a syntax error. Avoids needing to quote the
		// identifier everywhere, forever.
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "authorization_svc"),
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
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "authorization-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.authorization.events"),
			// Defaulted rather than opt-in: the topic name is
			// delegated-authority-svc's own compose default, so a stack
			// running both services projects delegations with no extra
			// configuration. Set KAFKA_DELEGATION_TOPIC="" to disable.
			// envAllowEmpty, not env: env() folds a set-but-empty variable
			// into its default, so with it there would be no way to express
			// "off" at all.
			DelegationTopic:   envAllowEmpty("KAFKA_DELEGATION_TOPIC", "zoiko.delegated-authority.events"),
			DelegationGroupID: env("KAFKA_DELEGATION_GROUP_ID", "authorization-svc-delegation-projector"),
		},
		JurisdictionRulesURL: env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8082"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		MTLSEnabled:              envBool("MTLS_ENABLED", false),
		MTLSPort:                 envInt("MTLS_PORT", 8449),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		SIEMServiceURL:           env("SIEM_SERVICE_URL", ""),
		PlatformScopeEntityID:    env("AUTHZ_PLATFORM_SCOPE_ENTITY_ID", ""),

		// Seconds, not a duration string: every other numeric knob in this
		// service is an int env var, and "5s" vs "5" is the kind of
		// difference that turns a cache into a no-op without an error.
		CacheTTL: time.Duration(envInt("AUTHZ_CACHE_TTL_SECONDS", 5)) * time.Second,

		// 24 months by default, which is a retention WINDOW rather than a
		// deletion: the function it configures detaches partitions, and an
		// operator archives them. 0 disables detaching entirely.
		AccessDecisionRetentionMonths: envInt("AUTHZ_ACCESS_DECISION_RETENTION_MONTHS", 24),
	}, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envAllowEmpty is env for a setting whose EMPTY value is meaningful.
//
// env() cannot express that: it treats a set-but-empty variable as unset and
// substitutes the default, so a feature defaulted to on can never be turned
// off by clearing its variable — the clearing silently re-enables it. This
// distinguishes "unset" from "set to empty" with LookupEnv.
func envAllowEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
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
