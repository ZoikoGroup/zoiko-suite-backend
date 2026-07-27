// Package config loads runtime configuration from environment variables for
// access-control-svc.
//
// All values have safe local-development defaults. In production the env vars
// must be explicitly set via Kubernetes Secrets / ConfigMaps.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for access-control-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8 Observability Baseline).
	OTELExporterEndpoint string
}

// DBConfig carries PostgreSQL connection parameters.
type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

// DSN builds a libpq-compatible connection string.
func (d DBConfig) DSN() string {
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

// KafkaConfig carries Kafka producer parameters.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables with safe defaults.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// Port 8119 — next available after all services in services/README.md
		// and the Phase 3-4 workforce services (8100-8118).
		Port: envInt("PORT", 8119),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "access_control_svc"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "access-control-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.access-control.events"),
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
