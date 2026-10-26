package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for policy-svc.
//
// AuthZServiceURL was absent until 2026-08-05 on the grounds that the
// Authorization Service "doesn't exist" — it has since shipped on :8089, so
// the deferral recorded here became a live gap: anyone able to reach this
// port could define a policy, publish a version, and activate it.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	// AuthZServiceURL is the base URL of authorization-svc. Every policy
	// mutation is authorized there before proceeding; no service
	// self-authorizes a material action (03-microservices.md §17.1).
	AuthZServiceURL string

	// AuthZPlatformScopeID is the legal_entity_id presented to
	// authorization-svc for a policy that is not scoped to one — a Policy
	// header row, or a version that applies tenant-wide or globally.
	// authorization-svc rejects an empty legal_entity_id, so those decisions
	// are evaluated against one synthetic platform-scope entity.
	AuthZPlatformScopeID string

	// GovernanceDecisionLogServiceURL is the base URL of
	// governance-decision-log-svc. The evidence relay (internal/evidence)
	// delivers every stored evaluation to POST /v1/decisions there.
	GovernanceDecisionLogServiceURL string

	// LedgerPrincipalID is the principal policy-svc writes to the governance
	// ledger as (decision D-02). It must hold GOVERNANCE_DECISION_RECORD.
	// Unset, evaluations still succeed and are stored, but stay PENDING.
	LedgerPrincipalID string

	// EvaluateAuthz and ReadAuthz gate POST /v1/policies/evaluate
	// (POLICY_EVALUATE) and the read routes (POLICY_READ): "enforce" refuses
	// without a GRANTED decision; "warn" checks and logs but admits, for the
	// migration window while access-control-svc registers the actions and
	// callers start forwarding X-Principal-Id. Anything else means enforce.
	EvaluateAuthz string
	ReadAuthz     string

	// SchedulerInterval is how often due scheduled activations are promoted.
	SchedulerInterval time.Duration

	// EvidenceRelayInterval is how often the evidence outbox is drained.
	EvidenceRelayInterval time.Duration

	// IdentityVerify is how the caller's identity headers are checked
	// (internal/identity): "enforce" (default) needs a signed envelope that
	// agrees with them, or an mTLS peer certificate; "warn" logs what enforce
	// would refuse; "off" trusts the headers (X5).
	IdentityVerify   string
	JWKSURL          string
	JWKSCacheTTL     time.Duration
	ExpectedIssuer   string
	ExpectedAudience string

	// MTLS* enable the mutual-TLS listener peers call (SEC-INV-05), the same
	// listener identity-context-svc and tenant-entity-registry-svc serve.
	MTLSEnabled            bool
	MTLSPort               int
	MTLSManagementURL      string
	MTLSBootstrapTokenPath string
	MTLSAllowedClients     []string

	// EventResidencyRegion is stamped on every published event's envelope.
	EventResidencyRegion string

	Kafka KafkaConfig

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

// KafkaConfig mirrors identity-context-svc's and tenant-entity-registry-svc's
// shape. GroupID is the consumer group for the consumed events (Doc 03 §8.1).
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

func (d DBConfig) DSN() string {
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8085, not 8084: 8080-8083 are taken (identity/tenant-entity/
		// jurisdiction-rules/governance-decision-log). 8084 is reserved
		// defensively for configuration-feature-flag-svc in case it lands
		// first — see PROGRESS.md.
		Port: envInt("PORT", 8085),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "policy"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		AuthZServiceURL:                 env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthZPlatformScopeID:            env("AUTHZ_PLATFORM_SCOPE_ID", ""),
		GovernanceDecisionLogServiceURL: env("GOVERNANCE_DECISION_LOG_SERVICE_URL", "http://governance-decision-log-svc:8083"),
		LedgerPrincipalID:               env("POLICY_LEDGER_PRINCIPAL_ID", ""),
		EvaluateAuthz:                   env("POLICY_EVALUATE_AUTHZ", "enforce"),
		ReadAuthz:                       env("POLICY_READ_AUTHZ", "enforce"),
		SchedulerInterval:               envDuration("POLICY_SCHEDULER_INTERVAL", 15*time.Second),
		EvidenceRelayInterval:           envDuration("POLICY_EVIDENCE_RELAY_INTERVAL", 2*time.Second),
		IdentityVerify:                  env("POLICY_IDENTITY_VERIFY", "enforce"),
		JWKSURL:                         env("IDENTITY_JWKS_URL", "http://identity-svc:8080/.well-known/jwks.json"),
		JWKSCacheTTL:                    envDuration("JWKS_CACHE_TTL", 5*time.Minute),
		ExpectedIssuer:                  env("EXPECTED_ISSUER", "identity-context-svc"),
		ExpectedAudience:                env("EXPECTED_AUDIENCE", "zoiko-internal"),
		MTLSEnabled:                     env("MTLS_ENABLED", "false") == "true",
		MTLSPort:                        envInt("MTLS_PORT", 8449),
		MTLSManagementURL:               env("MTLS_MANAGEMENT_SERVICE_URL", ""),
		MTLSBootstrapTokenPath:          env("MTLS_BOOTSTRAP_TOKEN_PATH", ""),
		MTLSAllowedClients:              splitList(env("MTLS_ALLOWED_CLIENTS", "")),
		EventResidencyRegion:            env("EVENT_RESIDENCY_REGION", ""),
		OTELExporterEndpoint:            env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "policy-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.policy.events"),
		},
	}, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
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
