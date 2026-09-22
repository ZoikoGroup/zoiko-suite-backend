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
	// mutual-TLS rollout. internal/mtls was pushed into this service as a
	// rollout target and then never referenced by anything: the package sat
	// here, complete and unreachable, so the service read as mTLS-capable in a
	// file listing and spoke plain HTTP to authorization-svc in every
	// deployment. Default off, matching the siblings — the point of this block
	// is that turning it on is now possible at all.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// ExpirySweepInterval bounds how long a delegation can outlive its own
	// effective_to before the background sweeper ends it and publishes
	// authority.expired. It is the headline operational number for this
	// service: identity-context-svc ends the delegate's session on that event,
	// so this is the window in which lapsed authority is still usable.
	//
	// Zero leaves the sweeper on its own default rather than disabling it.
	// There is deliberately no "off" switch here: before this loop existed,
	// expiry waited for somebody to read the register, and a tenant nobody read
	// expired nothing at all.
	ExpirySweepInterval time.Duration

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
		Port: envInt("PORT", 8136),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "delegated_authority"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "delegated-authority-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.delegated-authority.events"),
		},
		AuthZServiceURL: env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		ExpirySweepInterval: envDuration("EXPIRY_SWEEP_INTERVAL", 0),

		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
	}, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration reads a Go duration string ("30s", "2m").
//
// An unparseable value falls back to the default rather than failing startup,
// matching envInt above. The trade is deliberate and worth naming: a typo in
// EXPIRY_SWEEP_INTERVAL leaves the sweeper on its default interval instead of
// refusing to boot, so expiry keeps working at a slightly different cadence
// rather than the whole register going down over a malformed string.
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
