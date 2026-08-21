package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	AuthZServiceURL   string
	EmployeeMasterURL string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthZServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

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
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
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
		// 8139: 8135 belongs to vendor-due-diligence-svc, which is wired into
		// the console and routed there by Traefik. Both services published
		// 8135, so they could never both start.
		Port: envInt("PORT", 8139),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "performance_review"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "performance-review-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.performance-review.events"),
		},
		AuthZServiceURL:   env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		EmployeeMasterURL: env("EMPLOYEE_MASTER_URL", "http://employee-master-svc:8108"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

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
