package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for tenant-entity-registry-svc.
type Config struct {
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is the base URL of the Jurisdiction Rules Service.
	// Used by the JurisdictionValidator client for synchronous fail-closed validation
	// on EntityJurisdictionAssignment creation (Q2 resolution).
	JurisdictionRulesURL string

	// AuthZServiceURL is the base URL of the Authorization Service.
	// Every mutating API call must be authorized before proceeding.
	AuthZServiceURL string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string
}

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

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

func Load() (*Config, error) {
	return &Config{
		Port: envInt("PORT", 8081),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "tenant_entity_registry"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "tenant-entity-registry-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.entity.events"),
		},
		JurisdictionRulesURL: env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc"),
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
