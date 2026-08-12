package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for accounts-payable-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc. The Approve
	// transition is checked against it synchronously before applying — no
	// service self-authorizes (doctrine, 03-microservices.md). Fail-closed:
	// unreachable authorization-svc rejects the action, see
	// internal/authz.HTTPClient.
	AuthZServiceURL string

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

// Load reads configuration from environment variables, then refuses to start on
// a combination that would be unsafe outside local development.
//
// This used to end in `return cfg, nil` unconditionally — an error return that
// could never fire, which reads like validation while performing none. The
// dangerous values are all defaults that are *correct* locally and quietly wrong
// in production: an empty DB_PASSWORD, sslmode=disable, and a placeholder authz
// URL. Each would deploy successfully and fail as a security property rather
// than as a crash, which is the kind of failure nobody notices.
func Load() (*Config, error) {
	cfg := &Config{
		Env: env("ENV", "local"),
		// 8099: 8080-8098 are already taken by every other service built so
		// far (general-ledger-svc claimed 8098) — see services/README.md.
		Port: envInt("PORT", 8099),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "accounts_payable"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: splitBrokers(env("KAFKA_BROKERS", "localhost:9092")),
			GroupID: env("KAFKA_GROUP_ID", "accounts-payable-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.accounts-payable.events"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate enforces the invariants that only matter away from a developer
// machine. Local keeps every convenience; anything else has to be explicit.
func (c *Config) validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("PORT must be between 1 and 65535, got %d", c.Port)
	}
	if c.DB.Name == "" {
		return errors.New("DB_NAME must not be empty")
	}
	if c.AuthZServiceURL == "" {
		// Every mutation is gated on this call and fails closed, so an empty URL
		// does not silently permit anything — it refuses everything, which is a
		// service that starts healthy and cannot do its job.
		return errors.New("AUTHZ_SERVICE_URL must be set: every invoice mutation is authorized through it")
	}

	if c.Env == "local" || c.Env == "test" {
		return nil
	}

	if c.DB.Password == "" {
		return fmt.Errorf("DB_PASSWORD must not be empty when ENV=%s", c.Env)
	}
	if c.DB.SSLMode == "disable" {
		return fmt.Errorf("DB_SSLMODE must not be 'disable' when ENV=%s: this connection carries vendor payment data", c.Env)
	}
	if strings.Contains(c.AuthZServiceURL, "localhost") || strings.Contains(c.AuthZServiceURL, "127.0.0.1") {
		return fmt.Errorf("AUTHZ_SERVICE_URL points at %s when ENV=%s, which cannot be the real authorization-svc", c.AuthZServiceURL, c.Env)
	}
	if len(c.Kafka.Brokers) == 0 {
		// Locally an empty broker list selects a no-op publisher deliberately
		// (see cmd/server). In production that would mean silently publishing
		// none of the four §10.3 events, with nothing downstream to notice.
		return fmt.Errorf("KAFKA_BROKERS must not be empty when ENV=%s: vendor.invoice.* and payment.requested would never be published", c.Env)
	}
	return nil
}

// env reads a variable, treating "set but empty" as a real value.
//
// os.Getenv cannot distinguish unset from empty, so `KAFKA_BROKERS=` was
// replaced by the default and there was no way to ask for no brokers at all —
// which made the local no-broker path unreachable except by editing code.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// splitBrokers turns a comma-separated list into addresses, dropping blanks.
//
// strings.Split("", ",") returns []string{""} — one broker whose address is the
// empty string. That is worse than no brokers: the writer accepts it and every
// publish fails at dial time, one logged error per event.
func splitBrokers(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
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
