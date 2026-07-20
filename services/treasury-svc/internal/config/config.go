package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for treasury-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc.
	AuthZServiceURL string

	// LedgerServiceURL is the base URL of general-ledger-svc.
	LedgerServiceURL string

	// APServiceURL is the base URL of accounts-payable-svc.
	APServiceURL string

	// ARServiceURL is the base URL of accounts-receivable-svc.
	ARServiceURL string

	// ObligationsServiceURL is the base URL of obligations-svc.
	ObligationsServiceURL string

	// OTELExporterEndpoint is where telemetry sends trace data.
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

// KafkaConfig mirrors every other producer in this platform.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8103),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "treasury"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "treasury-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.treasury.events"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		LedgerServiceURL:     env("LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		APServiceURL:         env("AP_SERVICE_URL", "http://accounts-payable-svc:8099"),
		ARServiceURL:         env("AR_SERVICE_URL", "http://accounts-receivable-svc:8101"),
		ObligationsServiceURL: env("OBLIGATIONS_SERVICE_URL", "http://obligations-svc:8088"),
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
