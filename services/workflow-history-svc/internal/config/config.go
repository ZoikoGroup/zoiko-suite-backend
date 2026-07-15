// Package config holds runtime configuration for workflow-history-svc.
// All values are sourced from environment variables with safe defaults.
// No jurisdiction, currency, or tax-rule value is ever hardcoded here —
// see .agents/rules/doctrine.md.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for workflow-history-svc.
type Config struct {
	// Port is the HTTP port the service listens on.
	Port int

	// DB is the PostgreSQL connection configuration.
	DB DBConfig

	// Kafka is the consumer configuration.
	Kafka KafkaConfig

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string
}

// DBConfig carries the PostgreSQL connection parameters.
type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

// DSN returns a libpq-style connection string suitable for pgxpool.ParseConfig.
func (d DBConfig) DSN() string {
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

// KafkaConfig carries consumer configuration.
//
// This service subscribes to a single topic: zoiko.workflow.events,
// which carries all five workflow transition event types:
// workflow.started, approval.granted, approval.rejected,
// workflow.escalated, workflow.completed.
//
// TODO (production): add TLS, SASL (SCRAM-SHA-256), multi-broker retry, and
// consumer group lag metrics before production cutover.
type KafkaConfig struct {
	// Brokers is the list of Kafka bootstrap brokers.
	Brokers []string

	// GroupID is the Kafka consumer group identifier.
	GroupID string

	// Topic is the single Kafka topic this service subscribes to.
	Topic string
}

// Load reads config from the environment. It never returns an error in the
// current implementation (all fields have safe defaults), but the error return
// is preserved for future validation logic.
func Load() (*Config, error) {
	return &Config{
		Port: envInt("PORT", 8097),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "workflow_history"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "workflow-history-svc"),
			Topic:   env("KAFKA_TOPIC", "zoiko.workflow.events"),
		},
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
