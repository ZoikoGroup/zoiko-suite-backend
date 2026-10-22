package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for evidence-requirements-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc. Catalog
	// mutations (create/retire an evidence requirement) are checked against
	// it synchronously before applying — no service self-authorizes
	// (doctrine), and this service being part of the Governance Plane itself
	// is not an exemption. Fail-closed: unreachable authorization-svc
	// rejects the action, see internal/authz.
	AuthZServiceURL string

	// AuthzMTLSEnabled opts this service into calling authorization-svc over
	// mutual TLS instead of plain HTTP. OFF by default — see internal/mtls.
	AuthzMTLSEnabled bool
	AuthzMTLSURL     string

	MTLSManagementServiceURL string

	// DocumentVaultServiceURL is document-vault-svc's base URL. Artifacts
	// asserted with evidence_type SUPPORTING_DOCUMENT are verified against
	// the real Document record before they count as evidence, rather than
	// trusting the caller's claim (see internal/documentvault, and
	// context.md §11.2 for why this decision went the strict way).
	DocumentVaultServiceURL string

	// TenantRegistryURL is tenant-entity-registry-svc's base URL: the legal
	// entity an evaluation or requirement names is confirmed there (exists,
	// in the caller's tenant), fail-closed. Required outside local/dev.
	TenantRegistryURL string

	// EvaluateAuthzMode gates POST /v1/evidence/evaluate on EVIDENCE_EVALUATE
	// at the legal entity: "enforce" (default) or "warn" (log a refusal and
	// admit, while the gated callers' principals are being granted).
	EvaluateAuthzMode string

	// ReadAuthzMode gates the catalogue and ledger reads on
	// EVIDENCE_REQUIREMENT_READ / EVIDENCE_EVALUATION_READ: "enforce"
	// (default) or "warn".
	ReadAuthzMode string

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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8130: 8080-8129 are already claimed by every service built so far
		// (purchase-order-svc took 8129) — see deployments/docker-compose.yml.
		Port: envInt("PORT", 8130),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "evidence_requirements"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "evidence-requirements-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.evidence-requirements.events"),
		},
		AuthZServiceURL:          env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		DocumentVaultServiceURL:  env("DOCUMENT_VAULT_SERVICE_URL", "http://document-vault-svc:8094"),
		TenantRegistryURL:        os.Getenv("TENANT_REGISTRY_URL"),
		EvaluateAuthzMode:        strings.ToLower(env("EVIDENCE_EVALUATE_AUTHZ", "enforce")),
		ReadAuthzMode:            strings.ToLower(env("EVIDENCE_READ_AUTHZ", "enforce")),
		OTELExporterEndpoint:     env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
	}, nil
}

// LocalEnv reports whether Env names a developer tier.
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
