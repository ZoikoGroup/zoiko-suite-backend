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

	// PackSigningKeyRef and PackSigningKeyFile configure the key that signs
	// compiled jurisdiction-pack artifacts (ZS-JUR-001 s21, s28). The key is
	// read from a FILE (a mounted secret), never from the environment, and
	// its public half must be registered in pack_signing_keys. Both unset
	// means the sign command is disabled (503), which is the safe default.
	PackSigningKeyRef  string
	PackSigningKeyFile string

	// PackCertMinReviews is the number of distinct independent approving
	// reviewers a pack version needs before it can be certified. ZS-JUR-001
	// requires independent review but leaves the count and roles to a
	// controlled decision, so the deployment states it (default 1, minimum 1).
	PackCertMinReviews int

	// ResolverEligibleStatuses are the pack statuses the runtime resolver
	// may use for new decisions. ZS-JUR-001 s23: only RELEASED is production
	// eligible; CERTIFIED ("not yet production eligible") is accepted ONLY
	// outside production and staging, so a lower environment can exercise
	// the resolver before the release workflow (Wave 7) exists.
	ResolverEligibleStatuses []string
	// ResolverCacheTTLSeconds bounds how long a verified artifact is reused.
	// It is also the longest a key revocation or withdrawal can go unnoticed
	// unless the cache is invalidated explicitly. 0 disables caching.
	ResolverCacheTTLSeconds int

	// ResolverRing and ResolverRegion say which rollout ring and region this
	// instance serves. The resolver then uses ONLY pack versions deployed to
	// that ring and region (s23). Both unset means no deployment gating and is
	// refused in production and staging, where it would bypass the rollout.
	ResolverRing   string
	ResolverRegion string

	// ResolverEnabled turns the runtime resolver on. It is OFF by default so an
	// existing deployment keeps starting exactly as before; switching it on in
	// production or staging then requires a ring and a region.
	ResolverEnabled bool

	// HotfixRetroSLAHours is how long a hotfix retrospective may stay open
	// before it is reported overdue (s24 leaves the SLA to a controlled
	// decision; default 120).
	HotfixRetroSLAHours int
	// PackCertAgeWarnDays marks a certification stale in the metrics (default 180).
	PackCertAgeWarnDays int
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

		PackSigningKeyRef:  env("PACK_SIGNING_KEY_REF", ""),
		PackSigningKeyFile: env("PACK_SIGNING_KEY_FILE", ""),
		PackCertMinReviews: envInt("PACK_CERT_MIN_REVIEWS", 1),

		ResolverEligibleStatuses: envList("RESOLVER_ELIGIBLE_STATUSES", []string{"RELEASED"}),
		ResolverCacheTTLSeconds:  envInt("RESOLVER_CACHE_TTL_SECONDS", 30),
		ResolverEnabled:          env("RESOLVER_ENABLED", "false") == "true",
		ResolverRing:             env("RESOLVER_RING", ""),
		ResolverRegion:           env("RESOLVER_REGION", ""),
		HotfixRetroSLAHours:      envInt("HOTFIX_RETRO_SLA_HOURS", 120),
		PackCertAgeWarnDays:      envInt("PACK_CERT_AGE_WARN_DAYS", 180),
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

	if c.PackCertMinReviews < 1 {
		return fmt.Errorf("PACK_CERT_MIN_REVIEWS must be at least 1, got %d", c.PackCertMinReviews)
	}

	if c.ResolverCacheTTLSeconds < 0 || c.ResolverCacheTTLSeconds > 3600 {
		return fmt.Errorf("RESOLVER_CACHE_TTL_SECONDS must be between 0 and 3600, got %d", c.ResolverCacheTTLSeconds)
	}
	if len(c.ResolverEligibleStatuses) == 0 {
		return fmt.Errorf("RESOLVER_ELIGIBLE_STATUSES must name at least one status")
	}
	prodLike := strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "staging")
	for i, s := range c.ResolverEligibleStatuses {
		s = strings.ToUpper(strings.TrimSpace(s))
		c.ResolverEligibleStatuses[i] = s
		switch s {
		case "RELEASED":
		case "CERTIFIED":
			if prodLike {
				return fmt.Errorf("RESOLVER_ELIGIBLE_STATUSES may not include CERTIFIED in %s: only RELEASED packs are production eligible", c.Env)
			}
		default:
			return fmt.Errorf("RESOLVER_ELIGIBLE_STATUSES: %q is not a status that may resolve (RELEASED, or CERTIFIED outside production)", s)
		}
	}

	if c.HotfixRetroSLAHours < 1 || c.PackCertAgeWarnDays < 1 {
		return fmt.Errorf("HOTFIX_RETRO_SLA_HOURS and PACK_CERT_AGE_WARN_DAYS must be at least 1")
	}
	if (c.ResolverRing == "") != (c.ResolverRegion == "") {
		return fmt.Errorf("RESOLVER_RING and RESOLVER_REGION must be set together")
	}
	if c.ResolverEnabled && c.ResolverRing == "" && (strings.EqualFold(c.Env, "production") || strings.EqualFold(c.Env, "staging")) {
		return fmt.Errorf("RESOLVER_RING and RESOLVER_REGION must be set in %s when the resolver is enabled: without them it would use every released pack and bypass the rollout rings", c.Env)
	}

	// A half-configured signer is a misconfiguration in every environment:
	// refuse to start rather than discover it at the first release.
	if (c.PackSigningKeyRef == "") != (c.PackSigningKeyFile == "") {
		return fmt.Errorf("PACK_SIGNING_KEY_REF and PACK_SIGNING_KEY_FILE must be set together")
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
