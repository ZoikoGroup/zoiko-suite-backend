package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for policy-svc.
//
// No AuthZServiceURL field: admin writes (create/activate) do not call
// Authorization Service yet — it doesn't exist. This is a deliberate,
// documented deferral (see CONTEXT.md §11/§13), not an oversight, and
// matches governance-decision-log-svc's precedent of shipping without it.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	// GovernanceDecisionLogServiceURL is the base URL of
	// governance-decision-log-svc. Evaluate calls POST /v1/decisions there
	// after every evaluation to satisfy the "preserve evaluation basis for
	// governed decisions" evidence obligation (03-microservices.md §8.1).
	// Called synchronously but treated as best-effort — a failure here is
	// logged, not surfaced (see internal/decisionlog.HTTPClient doc comment).
	GovernanceDecisionLogServiceURL string

	Kafka KafkaConfig

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string
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

// KafkaConfig mirrors identity-context-svc's and tenant-entity-registry-svc's
// shape exactly. GroupID is unused today (this service only produces), kept
// for shape consistency with the rest of the platform.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
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
		Env: env("ENV", "local"),
		// 8085, not 8084: 8080-8083 are taken (identity/tenant-entity/
		// jurisdiction-rules/governance-decision-log). 8084 is reserved
		// defensively for configuration-feature-flag-svc in case it lands
		// first — see PROGRESS.md.
		Port: envInt("PORT", 8085),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "policy"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		GovernanceDecisionLogServiceURL: env("GOVERNANCE_DECISION_LOG_SERVICE_URL", "http://governance-decision-log-svc:8083"),
		OTELExporterEndpoint:            env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "policy-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.policy.events"),
		},
	}, nil
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
