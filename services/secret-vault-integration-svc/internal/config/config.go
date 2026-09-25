package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for secret-vault-integration-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc. Every mutating
	// API call is authorized there before proceeding; no service
	// self-authorizes a material action (03-microservices.md §17.1).
	AuthZServiceURL string

	// AuthZPlatformScopeID is the legal_entity_id presented to
	// authorization-svc when the mutation is not scoped to one.
	// authorization-svc rejects an empty legal_entity_id outright.
	AuthZPlatformScopeID string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthZServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// VaultKeyPath is where the v1 LocalFileVaultBackend persists its
	// encrypted-at-rest secret material. Production replaces this whole
	// backend with a real HashiCorp Vault / cloud KMS client — see
	// context.md §7.6. This is the same class of local-file bootstrap
	// compromise identity-context-svc's JWT_SIGNING_PRIVATE_KEY_PATH
	// already accepts.
	VaultKeyPath string

	// VaultMasterKeyHex is the AES-256 key (32 bytes, hex-encoded) used
	// to encrypt/decrypt secret material at rest. Never has a default —
	// must be supplied, or the backend refuses to start.
	//
	// Storing a raw secret in the environment is itself the SEC-INV-07
	// violation the compliance audit flagged, so this value is deprecated
	// in favour of VaultMasterKeyFile. Raw-hex still works in local dev but
	// is refused in staging/production unless ALLOW_PLAINTEXT_MASTER_KEY is
	// explicitly set to true (see server startup).
	VaultMasterKeyHex string

	// VaultMasterKeyFile is the preferred source of the AES-256 master key:
	// a path to a file whose entire trimmed contents are the 32-byte hex
	// key. The file must be readable only by its owner (0600) — the
	// backend refuses to start on a world/group-readable key file, since a
	// key file that any local user can read is no key isolation at all.
	VaultMasterKeyFile string

	// TLSCertFile/TLSKeyFile, when set, serve inbound HTTPS. Required in
	// staging/production unless ALLOW_INSECURE_INBOUND=true.
	TLSCertFile string
	TLSKeyFile  string
	// TLSClientCAFile, when set, enables mutual TLS on inbound requests:
	// clients must present a certificate signed by this CA.
	TLSClientCAFile string
	// MTLSIdentityCheck, when true, revalidates the presented client
	// certificate's identity (CN/SAN) against the X-Workload-Id /
	// X-Principal-Id header so a cert issued to one workload cannot be
	// presented as another.
	MTLSIdentityCheck bool

	// RotationSweepInterval is how often the automated rotation sweeper
	// runs (0 = disabled). See internal/rotation.
	RotationSweepInterval time.Duration

	// RotationSweepActor is the platform-scoped principal recorded as the
	// actor of every automated rotation.
	RotationSweepActor string

	// AllowInsecureInbound permits running without inbound TLS in
	// staging/production. False (default) refuses those environments when
	// no TLS_CERT_FILE/TLS_KEY_FILE is set.
	AllowInsecureInbound bool

// AllowPlaintextMasterKey permits the deprecated VAULT_MASTER_KEY_HEX
// env-var key in staging/production. False (default) requires
// VAULT_MASTER_KEY_FILE there.
	AllowPlaintextMasterKey bool

	// MaxLeaseDurationSeconds is the platform-wide ceiling for
	// max_lease_duration_seconds on secret policy versions. A version
	// requesting a longer lease is rejected at creation time. Default 24h.
	// 0 means no ceiling (not recommended for production).
	MaxLeaseDurationSeconds int

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string
}

// KafkaConfig holds event backbone connection parameters.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8087),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "secret_vault_integration"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		VaultKeyPath:         env("VAULT_LOCAL_STORE_PATH", "./secret_store.local"),
		VaultMasterKeyHex:    env("VAULT_MASTER_KEY_HEX", ""),
		VaultMasterKeyFile:   env("VAULT_MASTER_KEY_FILE", ""),
		TLSCertFile:          env("TLS_CERT_FILE", ""),
		TLSKeyFile:           env("TLS_KEY_FILE", ""),
		TLSClientCAFile:      env("TLS_CLIENT_CA_FILE", ""),
		MTLSIdentityCheck:    env("MTLS_IDENTITY_CHECK", "false") == "true",
		RotationSweepInterval: envDuration("ROTATION_SWEEP_INTERVAL", 0),
		RotationSweepActor:    env("ROTATION_SWEEP_ACTOR", "system:rotation-sweeper"),
		AllowInsecureInbound:    env("ALLOW_INSECURE_INBOUND", "false") == "true",
		AllowPlaintextMasterKey: env("ALLOW_PLAINTEXT_MASTER_KEY", "false") == "true",
		MaxLeaseDurationSeconds: envInt("MAX_LEASE_DURATION_SECONDS", 86400), // 24h default
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc"),
		AuthZPlatformScopeID: env("AUTHZ_PLATFORM_SCOPE_ID", ""),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		Kafka: KafkaConfig{
			Brokers: envList("KAFKA_BROKERS", []string{"localhost:9092"}),
			GroupID: env("KAFKA_GROUP_ID", "secret-vault-integration-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.secretvault.events"),
		},
	}, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envList reads a comma-separated list, trimming blanks. Unlike env(), an
// explicitly empty value is honoured rather than replaced by the default:
// KAFKA_BROKERS= is how a single-service local run says "no event backbone".
func envList(key string, def []string) []string {
	raw, set := os.LookupEnv(key)
	if !set {
		return def
	}
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
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

// envDuration reads a time.Duration (e.g. "24h"). Returns def (including an
// explicit 0 = disabled) on absence.
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
