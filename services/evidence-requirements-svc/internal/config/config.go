package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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

	// EvidenceManifestServiceURL is evidence-manifest-svc's base URL.
	// Artifacts asserted with evidence_type EVIDENCE_MANIFEST are verified
	// against the real Manifest record before they count as evidence.
	// Required outside local/dev; unset locally means the type is verified
	// as unavailable (503). It never silently counts.
	EvidenceManifestServiceURL string

	// EsignatureServiceURL is esignature-integration-svc's base URL.
	// Artifacts asserted with evidence_type SIGNATURE are verified against
	// the real Envelope record before they count as evidence.
	// Required outside local/dev; unset locally means the type is verified
	// as unavailable (503). It never silently counts.
	EsignatureServiceURL string

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

	// CursorSigningKey signs the next_cursor of every console list (API-4),
	// binding it to the tenant, route and filter that minted it.
	// CURSOR_SIGNING_KEY_HEX, at least 32 bytes; required outside local/dev.
	// Local and dev generate one per process, which only means a cursor does
	// not survive a restart. Same contract as identity-context-svc.
	CursorSigningKey []byte
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
	cfg := &Config{
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
		AuthZServiceURL:            env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:           env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:               env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL:   env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		DocumentVaultServiceURL:    env("DOCUMENT_VAULT_SERVICE_URL", "http://document-vault-svc:8094"),
		EvidenceManifestServiceURL: env("EVIDENCE_MANIFEST_SERVICE_URL", ""),
		EsignatureServiceURL:       env("ESIGNATURE_SERVICE_URL", ""),
		TenantRegistryURL:          os.Getenv("TENANT_REGISTRY_URL"),
		EvaluateAuthzMode:          strings.ToLower(env("EVIDENCE_EVALUATE_AUTHZ", "enforce")),
		ReadAuthzMode:              strings.ToLower(env("EVIDENCE_READ_AUTHZ", "enforce")),
		OTELExporterEndpoint:       env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
	}

	key, err := cursorKey(os.Getenv("CURSOR_SIGNING_KEY_HEX"), cfg.LocalEnv())
	if err != nil {
		return nil, err
	}
	cfg.CursorSigningKey = key
	return cfg, nil
}

// cursorKey decodes CURSOR_SIGNING_KEY_HEX. Unset is an error outside
// local/dev, where a per-process random key would make every next_cursor
// fail after a restart or on another replica.
func cursorKey(raw string, local bool) ([]byte, error) {
	if raw == "" {
		if !local {
			return nil, fmt.Errorf("CURSOR_SIGNING_KEY_HEX is required outside local/dev")
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate cursor signing key: %w", err)
		}
		return key, nil
	}
	key, err := hex.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("CURSOR_SIGNING_KEY_HEX is not hex: %w", err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("CURSOR_SIGNING_KEY_HEX must be at least 32 bytes, got %d", len(key))
	}
	return key, nil
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
