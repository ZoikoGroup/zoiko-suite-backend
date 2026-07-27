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

	AuthZServiceURL        string
	LedgerServiceURL       string
	IntercompanyServiceURL string

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
		Port: envInt("PORT", 8106),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "consolidation_svc"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "consolidation-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.consolidation.events"),
		},
		AuthZServiceURL:        env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		LedgerServiceURL:       env("LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		IntercompanyServiceURL: env("INTERCOMPANY_SERVICE_URL", "http://intercompany-accounting-svc:8105"),
		OTELExporterEndpoint:   env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
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