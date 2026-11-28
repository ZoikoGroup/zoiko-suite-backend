package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for obligations-svc.
//
// AuthZServiceURL exists now. It used not to, under this comment:
//
//	"No AuthZServiceURL field: admin writes do not call Authorization Service
//	 yet — it doesn't exist. Deliberate, documented deferral matching
//	 policy-svc's and governance-decision-log-svc's precedent."
//
// Both halves of that had gone stale. authorization-svc is live on :8089 and
// fifteen other services call it, and both services cited as precedent have
// since been wired to it. What the deferral actually left behind was an open
// write surface on a statutory compliance register: anything that could reach
// the port could raise an obligation, close one, or file against one.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is the base URL of jurisdiction-rules-svc. Called
	// synchronously on obligation creation to validate jurisdiction_id
	// (critical constraint: every obligation must be jurisdiction-bound) —
	// see internal/jurisdiction.HTTPValidator.
	JurisdictionRulesURL string

	// AuthZServiceURL is authorization-svc's base URL. Every mutation is
	// gated through it, fail-closed.
	AuthZServiceURL string

	// TenantRegistryURL is tenant-entity-registry-svc's base URL. Every legal
	// entity a write names is confirmed there (exists, and in the caller's
	// tenant), fail-closed. Empty disables the check, which only ENV
	// local/dev/test allows.
	TenantRegistryURL string

	// EvidenceRequirementsURL is evidence-requirements-svc's base URL. Called
	// before CLOSED transition to verify evidence sufficiency (Doc 03 §8.6).
	// Empty disables the check, which only ENV local/dev/test allows.
	EvidenceRequirementsURL string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// WorkflowURL is workflow-svc's base URL: waiver / not-applicable
	// approvals are verified there and escalation cases are opened there
	// (R-001 WFC-05, OB-S10/OB-S11). Empty refuses every waiver and leaves
	// case requests queued in the outbox; only local/dev allows it.
	WorkflowURL string

	// CalendarTopic carries jurisdiction-rules-svc's calendar events
	// (business-calendar.updated, jurisdiction.calendar.changed); a published
	// correction recalculates the clocks on the older version (OB-S8). Empty
	// disables the consumer.
	CalendarTopic string

	// ClockSweepInterval is how often the clock sweep persists computed
	// moves and fires due milestones. Reads never wait for it (R-INV-26).
	ClockSweepIntervalSeconds int

	// ServicePrincipal is the principal system workers act as (calendar
	// recalculation, escalation case relay); it needs the grants those calls
	// check, by migration (rule R5).
	ServicePrincipal string

	// Decision capture (DL-5, 25 Nov 2026): evaluate and waive decisions are
	// queued in decision_log_outbox (000017) and relayed to
	// governance-decision-log-svc (GOVERNANCE_DECISION_LOG_MTLS_URL) as
	// LedgerPrincipalID with this service's own workload certificate. An
	// empty URL leaves records waiting; an empty principal queues nothing.
	DecisionLogMTLSURL       string
	LedgerPrincipalID        string
	MTLSManagementServiceURL string
	MTLSBootstrapTokenPath   string
	LedgerRelayIntervalMS    int
	// LedgerRelayMaxPerSecond caps deliveries (decisionledger.SetMaxPerSecond).
	LedgerRelayMaxPerSecond int
	LedgerRetentionHours    int
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

// KafkaConfig mirrors identity-context-svc's, tenant-entity-registry-svc's,
// and policy-svc's shape exactly. GroupID is unused today (this service
// only produces), kept for shape consistency with the rest of the platform.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8088: 8080-8087 are already taken by every other Phase 1 service
		// built so far — secret-vault-integration-svc claimed 8087 while this
		// service was in flight on a separate branch — see services/README.md.
		Port: envInt("PORT", 8088),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "obligations"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "obligations-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.obligations.events"),
		},
		JurisdictionRulesURL:      env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8082"),
		AuthZServiceURL:           env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		TenantRegistryURL:         os.Getenv("TENANT_REGISTRY_URL"),
		EvidenceRequirementsURL:   os.Getenv("EVIDENCE_REQUIREMENTS_URL"),
		OTELExporterEndpoint:      env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		WorkflowURL:               os.Getenv("WORKFLOW_SERVICE_URL"),
		CalendarTopic:             os.Getenv("KAFKA_CALENDAR_TOPIC"),
		ClockSweepIntervalSeconds: envInt("CLOCK_SWEEP_INTERVAL_SECONDS", 60),
		ServicePrincipal:          env("SERVICE_PRINCIPAL_ID", "svc-obligations"),
		DecisionLogMTLSURL:        env("GOVERNANCE_DECISION_LOG_MTLS_URL", ""),
		LedgerPrincipalID:         env("LEDGER_PRINCIPAL_ID", "svc-obligations"),
		MTLSManagementServiceURL:  env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		MTLSBootstrapTokenPath:    env("MTLS_BOOTSTRAP_TOKEN_PATH", ""),
		LedgerRelayIntervalMS:     envInt("LEDGER_RELAY_INTERVAL_MS", 2000),
		LedgerRelayMaxPerSecond:   envInt("LEDGER_RELAY_MAX_PER_SECOND", 20),
		LedgerRetentionHours:      envInt("LEDGER_RETENTION_HOURS", 168),
	}, nil
}

// LocalEnv reports whether Env names a developer tier, where a dependency
// may be left unconfigured.
func (c *Config) LocalEnv() bool {
	switch strings.ToLower(c.Env) {
	case "local", "dev", "development", "test":
		return true
	}
	return false
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
