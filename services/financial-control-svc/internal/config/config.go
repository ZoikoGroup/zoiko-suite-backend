package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for financial-control-svc.
type Config struct {
	Env  string
	Port int

	DB    DBConfig
	Kafka KafkaConfig

	// AuthZServiceURL is authorization-svc. Every material control command is
	// checked against it, fail-closed (ZS-CONTROL-001 §25).
	AuthZServiceURL          string
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	OTELExporterEndpoint string

	// SourceEndpoints maps a source system name to its base URL, e.g.
	// "accounts-receivable=http://accounts-receivable-svc:8101". Only registered
	// systems can be read; a request can never supply a URL.
	SourceEndpoints string
	// Required ZS-EVENT-001 envelope attributes whose values are governed decisions; blank publishes UNSPECIFIED.
	EventResidencyRegion string
	EventClassification  string
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
		Env: env("ENV", "local"),
		// 8171: next free port after comments-collaboration-svc (8170).
		Port: envInt("PORT", 8171),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "financial_control"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "financial-control-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.financial-control.events"),
		},
		AuthZServiceURL:          env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		OTELExporterEndpoint:     env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		SourceEndpoints:          env("SOURCE_ENDPOINTS", ""),
		EventResidencyRegion:     env("EVENT_RESIDENCY_REGION", ""),
		EventClassification:      env("EVENT_CLASSIFICATION", ""),
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
