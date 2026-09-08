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

	AuthZServiceURL string
	// LedgerServiceURL is general-ledger-svc's own base URL — AST-02/AST-03
	// post through its ACC-04 system-originated posting path
	// (POST /v1/postings/events), never a bespoke ledger write.
	LedgerServiceURL string
	// CloseServiceURL is financial-close-svc's own base URL — AST-02
	// depends on its ACC-06 subledger control per the spec's own
	// Dependencies field.
	CloseServiceURL string

	// AuthzMTLSEnabled turns on the mTLS pilot for calls to authorization-svc.
	// OFF by default — when false, nothing about the existing authz call
	// changes.
	AuthzMTLSEnabled bool
	// AuthzMTLSURL is authorization-svc's mTLS listener, used only when
	// AuthzMTLSEnabled is true.
	AuthzMTLSURL string
	// MTLSManagementServiceURL is where this service provisions its own
	// client-side mTLS identity, used only when AuthzMTLSEnabled is true.
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
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8167),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "asset_management"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "asset-management-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.asset.events"),
		},
		AuthZServiceURL:  env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		LedgerServiceURL: env("LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		CloseServiceURL:  env("CLOSE_SERVICE_URL", "http://financial-close-svc:8104"),

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
