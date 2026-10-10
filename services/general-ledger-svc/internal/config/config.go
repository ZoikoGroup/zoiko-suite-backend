package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for general-ledger-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// EventResidencyRegion is the residencyregion every event this service
	// emits carries (ZS-EVENT-001 §4: required, "authoritative regional
	// data-bearing context"). Deliberately NO default: a wrong region silently
	// stamped on every financial event is worse than refusing to start.
	// EVENT_RESIDENCY_REGION, a lower-case code such as "uk" or "eu-west".
	EventResidencyRegion string

	// AuthZServiceURL is the base URL of authorization-svc. Every mutating
	// journal action (create/validate/post/reverse) is checked against it
	// synchronously before applying — no service self-authorizes (doctrine,
	// 03-microservices.md). Fail-closed: unreachable authorization-svc
	// rejects the action, see internal/authz.HTTPClient.
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

	// CloseServiceURL is the base URL of financial-close-svc.
	CloseServiceURL string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// PeriodGateMode is the REF-05 posting-gate mode: "off" (default) or
	// "shadow" (consult + compare only; never changes any outcome). "enforce"
	// is NOT implemented in this phase and is rejected at startup.
	PeriodGateMode string
	// AccountingPeriodURL is accounting-period-svc (REF-05).
	AccountingPeriodURL string
	// PeriodGateShadowTimeout bounds each shadow resolve call.
	PeriodGateShadowTimeout time.Duration
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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	mode, err := ParsePeriodGateMode(os.Getenv("PERIOD_GATE_MODE"))
	if err != nil {
		return nil, err
	}
	timeout := 300 * time.Millisecond
	if v := strings.TrimSpace(os.Getenv("PERIOD_GATE_SHADOW_TIMEOUT")); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			return nil, fmt.Errorf("PERIOD_GATE_SHADOW_TIMEOUT %q must be a positive duration such as 300ms", v)
		}
		timeout = d
	}
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if region == "" {
		return nil, errors.New("EVENT_RESIDENCY_REGION is required (e.g. uk, eu-west): every emitted event must carry its residency region")
	}
	return &Config{
		EventResidencyRegion: region,
		Env:                  env("ENV", "local"),
		// 8098: 8080-8097 are already taken by every other service built so
		// far (search-indexer-svc claimed 8096, workflow-history-svc claimed
		// 8097 on a sibling in-flight branch) — see services/README.md.
		Port: envInt("PORT", 8098),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "general_ledger"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "general-ledger-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.general-ledger.events"),
		},
		AuthZServiceURL:          env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:         os.Getenv("AUTHZ_MTLS_ENABLED") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		CloseServiceURL:          env("CLOSE_SERVICE_URL", "http://financial-close-svc:8104"),
		OTELExporterEndpoint:     env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		PeriodGateMode:           mode,
		AccountingPeriodURL:      env("ACCOUNTING_PERIOD_URL", "http://accounting-period-svc:8174"),
		PeriodGateShadowTimeout:  timeout,
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

// ParsePeriodGateMode validates PERIOD_GATE_MODE. Only "off" (also when unset)
// and "shadow" are accepted; "enforce" gets a specific error because it is the
// obvious next value and silently accepting it would make operators believe
// the gate is enforcing when it is not.
func ParsePeriodGateMode(raw string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(raw)); m {
	case "", "off":
		return "off", nil
	case "shadow":
		return "shadow", nil
	case "enforce":
		return "", fmt.Errorf("PERIOD_GATE_MODE=enforce is not implemented in this phase: enforcement is phase 4 and requires separate sign-off; allowed values are off or shadow")
	default:
		return "", fmt.Errorf("invalid PERIOD_GATE_MODE %q: allowed values are off or shadow (enforce is not implemented in this phase)", raw)
	}
}
