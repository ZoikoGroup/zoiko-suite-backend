package config

import (
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

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string
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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
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
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "jurisdiction-rules-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "jurisdiction.rules"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
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
