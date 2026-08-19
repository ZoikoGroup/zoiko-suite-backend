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

	AuthZServiceURL string

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

// DSN builds the connection string from the DB_* variables and nothing else.
//
// It used to return TEST_DATABASE_URL when that variable was set, "for
// integration tests" — but no test calls this function (the store suite opens
// its own pool from that variable directly), so all it did was give the
// running service a way to silently connect somewhere other than its
// configured database if the variable ever leaked into an environment. The
// store suite DROPs tables. Same removal as financial-close-svc.
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
	cfg := &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8133),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "notification"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			// os.LookupEnv, not env(): KAFKA_BROKERS= (explicitly empty) is how
			// a deployment says "no broker — publish in dry mode". env() would
			// substitute the default and make that unreachable.
			Brokers: brokers(),
			GroupID: env("KAFKA_GROUP_ID", "notification-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.notification.events"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}

	// Load returned a nil error unconditionally, so every default above was
	// also a production default: an empty DB password, and an authz URL that
	// could point at a placeholder. Refuse to start rather than run a
	// governed service on them.
	if cfg.Env == "production" {
		var missing []string
		if cfg.DB.Password == "" {
			missing = append(missing, "DB_PASSWORD")
		}
		if cfg.DB.SSLMode == "disable" {
			missing = append(missing, "DB_SSLMODE (must not be 'disable' in production)")
		}
		if cfg.AuthZServiceURL == "" || strings.Contains(cfg.AuthZServiceURL, "localhost") {
			missing = append(missing, "AUTHZ_SERVICE_URL (must be a real authorization-svc address)")
		}
		if len(missing) > 0 {
			return nil, errors.New("invalid production config: " + strings.Join(missing, ", "))
		}
	}
	return cfg, nil
}

func brokers() []string {
	v, ok := os.LookupEnv("KAFKA_BROKERS")
	if !ok {
		return []string{"localhost:9092"}
	}
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, ",")
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
