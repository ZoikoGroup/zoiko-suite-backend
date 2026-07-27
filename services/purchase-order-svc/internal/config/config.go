package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for purchase-order-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc. Issue/Amend/Close
	// are all checked against it synchronously before applying — no service
	// self-authorizes (doctrine, 03-microservices.md). Fail-closed:
	// unreachable authorization-svc rejects the action, see internal/authz.
	AuthZServiceURL string

	// PurchaseRequestServiceURL is purchase-request-svc's base URL. When a
	// caller supplies purchase_request_id on Issue, this service verifies —
	// synchronously, fail-closed — that the referenced request is real,
	// belongs to the same tenant/legal entity, and is APPROVED, rather than
	// trusting the caller's claim (see internal/purchaserequest).
	PurchaseRequestServiceURL string

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

func (d DBConfig) DSN() string {
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

// KafkaConfig mirrors every other producer in this platform's shape exactly.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8129: 8080-8128 are already taken by every other service built so
		// far (corporate-tax-svc claimed 8128) — see docker-compose.yml.
		Port: envInt("PORT", 8129),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "purchase_order"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "purchase-order-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.purchase-order.events"),
		},
		AuthZServiceURL:           env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		PurchaseRequestServiceURL: env("PURCHASE_REQUEST_SERVICE_URL", "http://purchase-request-svc:8100"),
		OTELExporterEndpoint:      env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
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
