package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// EventResidencyRegion is the residencyregion every event this service
	// emits carries (ZS-EVENT-001 §4: required, "authoritative regional
	// data-bearing context"). Deliberately NO default: a wrong region
	// silently stamped on every financial event is worse than refusing to
	// start. EVENT_RESIDENCY_REGION, a lower-case code such as "uk" or
	// "eu-west".
	EventResidencyRegion string

	AuthZServiceURL string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot. Disabled by default.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// FinancialCloseURL is financial-close-svc, owner of ACC-16 (Signed
	// Financial Snapshot) — the only source a statement set may be built from.
	FinancialCloseURL string

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
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if region == "" {
		return nil, errors.New("EVENT_RESIDENCY_REGION is required (e.g. uk, eu-west): every emitted event must carry its residency region")
	}

	return &Config{
		EventResidencyRegion: region,
		Env:                  env("ENV", "local"),
		Port:                 envInt("PORT", 8175),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "financial_statements"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "financial-statements-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.financial-statements.events"),
		},
		AuthZServiceURL: env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		FinancialCloseURL: env("FINANCIAL_CLOSE_URL", "http://financial-close-svc:8104"),

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
