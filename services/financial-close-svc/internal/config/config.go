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
	LedgerServiceURL         string
	APServiceURL             string
	ARServiceURL             string
	VaultServiceURL          string

	// CloseSigningKey is the HMAC secret the close evidence signature is
	// computed with. There is deliberately NO default: the signature used to be
	// keyed with the tenant ID, which is a public identifier, so every
	// signature this service had ever produced was forgeable by anyone who had
	// seen a request. A fallback here would reintroduce exactly that — a
	// signature that looks like a guarantee and is not — so an unset key is a
	// startup failure instead.
	CloseSigningKey string

	OTELExporterEndpoint string
}

// ErrSigningKeyMissing is returned by Load when CLOSE_SIGNING_KEY is unset.
type ErrSigningKeyMissing struct{}

func (ErrSigningKeyMissing) Error() string {
	return "CLOSE_SIGNING_KEY is required: close evidence is signed with it, and there is no safe default"
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
// integration tests" — but no test calls this function (the store suites open
// their own pool from that variable directly), so all it did was give the
// running service a way to silently connect somewhere other than its
// configured database if the variable ever leaked into an environment. The
// store suites DROP tables. A test-only override in production config is a
// footgun with no upside.
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
	signingKey := env("CLOSE_SIGNING_KEY", "")
	if signingKey == "" {
		return nil, ErrSigningKeyMissing{}
	}

	return &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8104),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "financial_close"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "financial-close-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.close.events"),
		},
		AuthZServiceURL:          env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:         os.Getenv("AUTHZ_MTLS_ENABLED") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		LedgerServiceURL:         env("LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		APServiceURL:             env("AP_SERVICE_URL", "http://accounts-payable-svc:8099"),
		ARServiceURL:             env("AR_SERVICE_URL", "http://accounts-receivable-svc:8101"),
		// 8094 is the port document-vault-svc listens on. This defaulted to
		// 8092, which nothing in this platform serves.
		VaultServiceURL:      env("VAULT_SERVICE_URL", "http://document-vault-svc:8094"),
		CloseSigningKey:      signingKey,
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
