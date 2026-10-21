package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for workflow-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthorizationServiceURL is called synchronously on every approval
	// action submission — see internal/authz.HTTPClient.
	AuthorizationServiceURL string

	// DocumentVaultServiceURL is used to verify acceptance evidence before an
	// audit engagement can enter acceptance review. The returned version is
	// pinned in the engagement record; workflow-svc never owns evidence bytes.
	DocumentVaultServiceURL string

	// GovernanceDecisionLogServiceURL is used to record governance decisions
	// (workflow approvals, rejections, escalations, cancellations, invalidations)
	// to the governance-decision-log-svc per GOV §1 "No evidence afterthought".
	GovernanceDecisionLogServiceURL string

	// LedgerPrincipalID is the workflow-svc service principal the outbox
	// ledger deliverer writes as. Empty leaves decision_log events pending.
	LedgerPrincipalID string

	// RegistryServiceURL is tenant-entity-registry-svc, the owner of the
	// legal-entity fact a workflow binds to. Empty disables the check (local
	// only); the handler then admits any entity in the caller's tenant.
	RegistryServiceURL string

	// ReadAuthzMode is WORKFLOW_READ_AUTHZ: enforce (default) | warn | off.
	ReadAuthzMode string

	// Consumer group and topics for the authority-change re-check consumer.
	ConsumerGroupID string
	ConsumerTopics  []string

	// TimerSweepInterval and TimerSweepBatch drive deadline expiry,
	// escalation and approver re-authorization.
	TimerSweepInterval time.Duration
	TimerSweepBatch    int

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8090: 8080-8089 are already taken by every other Phase 1 service
		// built so far — see services/README.md.
		Port: envInt("PORT", 8090),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "workflow"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "workflow-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.workflow.events"),
		},
		AuthorizationServiceURL:         env("AUTHORIZATION_SERVICE_URL", "http://authorization-svc:8089"),
		DocumentVaultServiceURL:         env("DOCUMENT_VAULT_SERVICE_URL", "http://document-vault-svc:8094"),
		GovernanceDecisionLogServiceURL: env("GOVERNANCE_DECISION_LOG_SERVICE_URL", "http://governance-decision-log-svc:8083"),
		LedgerPrincipalID:               env("WORKFLOW_LEDGER_PRINCIPAL_ID", ""),
		ReadAuthzMode:                   env("WORKFLOW_READ_AUTHZ", "enforce"),
		RegistryServiceURL:              env("TENANT_ENTITY_REGISTRY_SERVICE_URL", "http://tenant-entity-registry-svc:8081"),
		ConsumerGroupID:                 env("KAFKA_CONSUMER_GROUP_ID", "workflow-svc-recheck"),
		ConsumerTopics: envList("KAFKA_CONSUMED_TOPICS",
			"zoiko.policy.events,zoiko.delegated-authority.events,zoiko.authorization.events"),
		TimerSweepInterval:   envDuration("WORKFLOW_TIMER_SWEEP_INTERVAL", 30*time.Second),
		TimerSweepBatch:      envInt("WORKFLOW_TIMER_SWEEP_BATCH", 100),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
	}, nil
}

func envList(key, def string) []string {
	var out []string
	for _, item := range strings.Split(env(key, def), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
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
