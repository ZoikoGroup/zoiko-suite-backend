package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for configuration-feature-flag-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc. Every mutating
	// API call is authorized there before proceeding; no service
	// self-authorizes a material action (03-microservices.md §17.1).
	AuthZServiceURL string

	// AuthZPlatformScopeID is the legal_entity_id presented to
	// authorization-svc when the mutation is not scoped to one.
	// authorization-svc rejects an empty legal_entity_id outright.
	AuthZPlatformScopeID string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthZServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// SweepInterval is how often the expiry sweep runs. The sweep flips
	// expired kill switches and expires overdue emergency changes and emits
	// config.emergency.expired for each; it is background bookkeeping, so the
	// default is a minute, not the request path's cadence.
	SweepInterval time.Duration

	// SweepEnvironments is the list of environments the expiry sweep covers.
	// Environments are free-form strings in the data model — there is no
	// environments table to enumerate them from — so the sweep must be told
	// which ones carry time-boxed objects.
	SweepEnvironments []string
}

// KafkaConfig holds event backbone connection parameters.
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

// ErrPlatformScopeMissing is returned when AUTHZ_PLATFORM_SCOPE_ID is unset
// outside local development.
//
// It is fatal rather than defaulted because of how the failure presents
// otherwise. Every write authorizes against this value as the legal_entity_id,
// and authorization-svc rejects an empty one outright — so an unset variable
// does not disable the check, it makes every single write fail with an error
// about authorization being unavailable, while every read keeps working. The
// cause is a missing environment variable and the symptom is what a dependency
// outage looks like.
var ErrPlatformScopeMissing = errors.New("AUTHZ_PLATFORM_SCOPE_ID is required: every write authorizes against it as the legal_entity_id, and authorization-svc refuses an empty one — leaving it unset fails every write with an error that reads as an authorization-svc outage")

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Env:                  env("ENV", "local"),
		Port:                 envInt("PORT", 8086),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthZPlatformScopeID: env("AUTHZ_PLATFORM_SCOPE_ID", ""),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		Kafka: KafkaConfig{
			Brokers: envList("KAFKA_BROKERS", []string{"localhost:9092"}),
			GroupID: env("KAFKA_GROUP_ID", "configuration-feature-flag-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.configuration.events"),
		},
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "configuration_feature_flag"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		SweepInterval:     envDuration("SWEEP_INTERVAL", time.Minute),
		SweepEnvironments: envList("SWEEP_ENVIRONMENTS", []string{"staging", "production"}),
	}

	// Local development is allowed to omit it — a single-service run with no
	// authorization-svc at all is a normal state there, and authz.NewClient
	// already refuses a placeholder URL outside local. Anywhere that claims to
	// be a deployment must name the scope.
	if cfg.AuthZPlatformScopeID == "" && !strings.EqualFold(cfg.Env, "local") {
		return nil, ErrPlatformScopeMissing
	}

	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envList reads a comma-separated list, trimming blanks. Unlike env(), an
// explicitly empty value is honoured rather than replaced by the default:
// KAFKA_BROKERS= is how a single-service local run says "no event backbone".
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

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
