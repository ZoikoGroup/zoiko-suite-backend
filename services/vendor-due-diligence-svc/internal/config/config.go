package config

import (
	"errors"
	"fmt"
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
	CounterpartyServiceURL string

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

// Load reads configuration from the environment, then refuses to start on a
// combination that would be unsafe outside local development.
//
// This used to end in `return cfg, nil` unconditionally — an error return that
// could never fire, which reads like validation while performing none. The
// dangerous values are all defaults that are correct locally and quietly wrong in
// production: an empty DB_PASSWORD, sslmode=disable, and service URLs pointing at
// localhost. Each would deploy successfully and fail as a security property
// rather than as a crash.
func Load() (*Config, error) {
	cfg := &Config{
		Env: env("ENV", "local"),
		// 8135, which is what deployments/docker-compose.yml publishes and sets
		// PORT to. The default was 8132 — a port nothing in this platform uses —
		// so the service was reachable only because compose overrode it, and any
		// bring-up without that override listened somewhere nobody was calling.
		Port: envInt("PORT", 8135),

		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "vendor_due_diligence"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: splitBrokers(env("KAFKA_BROKERS", "localhost:9092")),
			GroupID: env("KAFKA_GROUP_ID", "vendor-due-diligence-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.vendor-due-diligence.events"),
		},

		AuthZServiceURL:        env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		CounterpartyServiceURL: env("COUNTERPARTY_SERVICE_URL", "http://counterparty-management-svc:8124"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

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
		// Every route is gated on this call and fails closed, so an empty URL does
		// not silently permit anything — it refuses everything, which is a service
		// that starts healthy and cannot do its job.
		return errors.New("AUTHZ_SERVICE_URL must be set: every vendor due diligence action is authorized through it")
	}
	if c.CounterpartyServiceURL == "" {
		// Unlike authz this is best-effort, so an empty URL does not refuse
		// anything — it silently stops every outcome reaching the counterparty
		// record while every check still answers 201. Requiring it makes that
		// choice explicit rather than a typo.
		return errors.New("COUNTERPARTY_SERVICE_URL must be set: completed checks are pushed to counterparty-management-svc")
	}

	if c.Env == "local" || c.Env == "test" {
		return nil
	}

	if c.DB.Password == "" {
		return fmt.Errorf("DB_PASSWORD must not be empty when ENV=%s", c.Env)
	}
	if c.DB.SSLMode == "disable" {
		return fmt.Errorf("DB_SSLMODE must not be 'disable' when ENV=%s: this connection carries screening outcomes and due diligence evidence", c.Env)
	}
	for name, url := range map[string]string{
		"AUTHZ_SERVICE_URL":        c.AuthZServiceURL,
		"COUNTERPARTY_SERVICE_URL": c.CounterpartyServiceURL,
	} {
		if strings.Contains(url, "localhost") || strings.Contains(url, "127.0.0.1") {
			return fmt.Errorf("%s points at %s when ENV=%s, which cannot be the real service", name, url, c.Env)
		}
	}
	if len(c.Kafka.Brokers) == 0 {
		// Locally an empty broker list selects the log-only path deliberately. In
		// production it would mean silently publishing none of the screening
		// events, with nothing downstream to notice that a vendor had been flagged.
		return fmt.Errorf("KAFKA_BROKERS must not be empty when ENV=%s: vendor.dd.started, .completed and .failed would never be published", c.Env)
	}
	return nil
}

// env reads a variable, treating "set but empty" as a real value. os.Getenv
// cannot distinguish unset from empty, so `KAFKA_BROKERS=` was replaced by the
// default and there was no way to ask for no brokers at all — making the
// log-only publisher path unreachable.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// splitBrokers turns a comma-separated list into addresses, dropping blanks.
// strings.Split("", ",") returns one broker whose address is the empty string,
// which the writer accepts and then fails to dial on every publish.
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
