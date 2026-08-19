package config

import (
	"os"
	"strconv"
	"strings"
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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
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
	}, nil
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
