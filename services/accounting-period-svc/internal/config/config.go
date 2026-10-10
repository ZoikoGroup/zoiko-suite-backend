package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	AuthZServiceURL string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mutual-TLS rollout. Default off, matching the siblings.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// FiscalCalendarURL is fiscal-calendar-svc (REF-04), owner of period boundaries.
	FiscalCalendarURL string
	// FinancialCloseURL is financial-close-svc (hosts ACC-14 workflow evidence).
	// GET {FinancialCloseURL}/v1/close/workflow-refs/{ref} DOES NOT EXIST YET (built
	// in the later cutover step): until it does, every state command fails closed.
	FinancialCloseURL string
	// ReopenMaxWindow bounds how far ahead an authorised reopen may expire.
	ReopenMaxWindow time.Duration

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
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8174),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "accounting_period"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "accounting-period-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.accounting-period.events"),
		},
		AuthZServiceURL: env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		FiscalCalendarURL: env("FISCAL_CALENDAR_URL", "http://fiscal-calendar-svc:8173"),
		FinancialCloseURL: env("FINANCIAL_CLOSE_URL", "http://financial-close-svc:8104"),
		ReopenMaxWindow:   envDuration("REOPEN_MAX_WINDOW", 72*time.Hour),

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

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
