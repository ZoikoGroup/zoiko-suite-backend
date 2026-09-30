package config

import (
	"fmt"
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
	// MTLSBootstrapTokenPath is the provisioning token file mtls-management-svc
	// authenticates self-provisioning services by (MTLS_BOOTSTRAP_TOKEN_PATH).
	MTLSBootstrapTokenPath string

	// VaultKeyPath is where the v1 LocalFileVaultBackend persists its
	// encrypted-at-rest secret material. Production replaces this whole
	// backend with a real HashiCorp Vault / cloud KMS client — see
	// context.md §7.6. This is the same class of local-file bootstrap
	// compromise identity-context-svc's JWT_SIGNING_PRIVATE_KEY_PATH
	// already accepts.
	VaultKeyPath string

	// VaultKEKProvider selects where the master key lives (SEC-INV-07):
	//   local   — in this process (VAULT_MASTER_KEY_FILE / _HEX); local dev
	//             only, refused in staging/production unless
	//             ALLOW_IN_PROCESS_MASTER_KEY=true.
	//   transit — HashiCorp Vault / OpenBao Transit (VAULT_TRANSIT_*).
	//   gcpkms  — Google Cloud KMS (GCP_KMS_KEY_NAME), as the workload's
	//             service account.
	// With transit/gcpkms the master key never enters this process; material
	// is envelope-encrypted under per-secret data keys the KMS wraps.
	VaultKEKProvider string

	VaultTransitAddr      string
	VaultTransitMount     string
	VaultTransitKey       string
	VaultTransitTokenFile string
	GCPKMSKeyName         string

	// VaultMigrateLegacyKeyFile, when set with a KMS provider, re-encrypts a
	// legacy local-key store file into the envelope format at startup (the
	// migration path off the in-process key; see vault.MigrateLocalStore).
	VaultMigrateLegacyKeyFile string

	// AllowInProcessMasterKey permits VAULT_KEK_PROVIDER=local in
	// staging/production — the documented, deliberate override.
	AllowInProcessMasterKey bool

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
	// presented as another. Defaults to true whenever TLS_CLIENT_CA_FILE is
	// set — mTLS whose certificate vouches for nothing in particular is
	// transport encryption, not workload identity. MTLS_IDENTITY_CHECK=false
	// still turns it off explicitly.
	MTLSIdentityCheck bool

	// MTLSMaxClientCertLifetime and MTLSRequiredURIPrefix are the §9
	// workload-certificate policy (see inboundmtls.CertPolicy). Off when
	// unset: the issuer cannot meet either yet.
	MTLSMaxClientCertLifetime time.Duration
	MTLSRequiredURIPrefix     string

	// MTLSTrustedForwarders are the certificate identities (CN/SAN) allowed
	// to forward a principal other than themselves — the gateway hop, which
	// sets X-Principal-Id from a token it verified (§9 "the receiving service
	// revalidates forwarded context": it revalidates the forwarder). Every
	// other certificate must name each identity header it sends.
	MTLSTrustedForwarders []string

	// RotationSweepInterval is how often the automated rotation sweeper
	// runs (0 = disabled). See internal/rotation.
	RotationSweepInterval time.Duration

	// RequireSharedSecretException makes the broker refuse a secret shared by
	// more than one workload unless an ACTIVE, unexpired shared-secret
	// exception documents it (§13 "shared production secrets require a
	// documented exception"). Defaults on in staging/production.
	RequireSharedSecretException bool

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
		VaultKEKProvider:          strings.ToLower(env("VAULT_KEK_PROVIDER", "local")),
		VaultTransitAddr:          env("VAULT_TRANSIT_ADDR", ""),
		VaultTransitMount:         env("VAULT_TRANSIT_MOUNT", "transit"),
		VaultTransitKey:           env("VAULT_TRANSIT_KEY", ""),
		VaultTransitTokenFile:     env("VAULT_TOKEN_FILE", ""),
		GCPKMSKeyName:             env("GCP_KMS_KEY_NAME", ""),
		VaultMigrateLegacyKeyFile: env("VAULT_MIGRATE_LEGACY_KEY_FILE", ""),
		AllowInProcessMasterKey:   env("ALLOW_IN_PROCESS_MASTER_KEY", "false") == "true",
		TLSCertFile:          env("TLS_CERT_FILE", ""),
		TLSKeyFile:           env("TLS_KEY_FILE", ""),
		TLSClientCAFile:      env("TLS_CLIENT_CA_FILE", ""),
		MTLSIdentityCheck:    identityCheckDefault(os.Getenv("MTLS_IDENTITY_CHECK"), os.Getenv("TLS_CLIENT_CA_FILE")),
		MTLSTrustedForwarders: envList("MTLS_TRUSTED_FORWARDERS", nil),
		MTLSMaxClientCertLifetime: envDuration("MTLS_MAX_CLIENT_CERT_LIFETIME", 0),
		MTLSRequiredURIPrefix:     env("MTLS_REQUIRED_URI_PREFIX", ""),
		// On by default (§13 "rotation must be automated"): it acts only on
		// versions that declare rotation_interval_seconds, so an estate with
		// no schedules pays one indexed query per interval. 0 disables.
		RotationSweepInterval: envDuration("ROTATION_SWEEP_INTERVAL", 15*time.Minute),
		RequireSharedSecretException: sharedSecretRuleDefault(os.Getenv("REQUIRE_SHARED_SECRET_EXCEPTION"), env("ENV", "local")),
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
		MTLSBootstrapTokenPath:   env("MTLS_BOOTSTRAP_TOKEN_PATH", ""),
		Kafka: KafkaConfig{
			Brokers: envList("KAFKA_BROKERS", []string{"localhost:9092"}),
			GroupID: env("KAFKA_GROUP_ID", "secret-vault-integration-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.secretvault.events"),
		},
	}, nil
}

// sharedSecretRuleDefault resolves REQUIRE_SHARED_SECRET_EXCEPTION: explicit
// wins; unset, on exactly in the environments that hold production secrets.
func sharedSecretRuleDefault(explicit, env string) bool {
	if explicit != "" {
		return explicit == "true"
	}
	return IsRestrictedEnvironment(env)
}

// identityCheckDefault resolves MTLS_IDENTITY_CHECK: an explicit value wins;
// unset, it is on exactly when inbound mTLS is.
func identityCheckDefault(explicit, clientCAFile string) bool {
	if explicit != "" {
		return explicit == "true"
	}
	return clientCAFile != ""
}

// ValidateInbound refuses an inbound transport that does not authenticate its
// peer in staging/production (SEC-INV-05, Security Standard §9: service-to-
// service calls use unique workload identity and authenticated transport).
// Server-only TLS used to satisfy this gate — encrypted, but any caller that
// could reach the port was accepted. ALLOW_INSECURE_INBOUND=true remains the
// documented, deliberate override.
func (c *Config) ValidateInbound() error {
	if c.TLSCertFile == "" && c.TLSKeyFile == "" && c.TLSClientCAFile != "" {
		return fmt.Errorf("TLS_CLIENT_CA_FILE is set but TLS_CERT_FILE/TLS_KEY_FILE are not: mTLS needs a server keypair")
	}
	if !IsRestrictedEnvironment(c.Env) || c.AllowInsecureInbound {
		return nil
	}
	if c.TLSCertFile == "" || c.TLSKeyFile == "" || c.TLSClientCAFile == "" {
		return fmt.Errorf("refusing to start in %s without inbound mutual TLS: set TLS_CERT_FILE, TLS_KEY_FILE and TLS_CLIENT_CA_FILE, or ALLOW_INSECURE_INBOUND=true to override", c.Env)
	}
	if !c.MTLSIdentityCheck {
		return fmt.Errorf("refusing to start in %s with MTLS_IDENTITY_CHECK=false: a client certificate must bind the identity the broker authorizes", c.Env)
	}
	return nil
}

// ValidateVault refuses a master key held in this process in
// staging/production (SEC-INV-07), and a KMS provider missing its settings
// anywhere.
func (c *Config) ValidateVault() error {
	switch c.VaultKEKProvider {
	case "local":
		if IsRestrictedEnvironment(c.Env) && !c.AllowInProcessMasterKey {
			return fmt.Errorf("refusing to start in %s with VAULT_KEK_PROVIDER=local: the master key would live in this process (SEC-INV-07); use transit or gcpkms, or ALLOW_IN_PROCESS_MASTER_KEY=true to override", c.Env)
		}
	case "transit":
		if c.VaultTransitAddr == "" || c.VaultTransitKey == "" || c.VaultTransitTokenFile == "" {
			return fmt.Errorf("VAULT_KEK_PROVIDER=transit needs VAULT_TRANSIT_ADDR, VAULT_TRANSIT_KEY and VAULT_TOKEN_FILE")
		}
	case "gcpkms":
		if c.GCPKMSKeyName == "" {
			return fmt.Errorf("VAULT_KEK_PROVIDER=gcpkms needs GCP_KMS_KEY_NAME (projects/.../cryptoKeys/...)")
		}
	default:
		return fmt.Errorf("VAULT_KEK_PROVIDER=%q: want local, transit or gcpkms", c.VaultKEKProvider)
	}
	return nil
}

// ValidateLeaseCeiling refuses an unbounded lease ceiling in
// staging/production: SEC-INV-12 access is time-bound, and 0 meant any policy
// could declare any lease duration.
func (c *Config) ValidateLeaseCeiling() error {
	if c.MaxLeaseDurationSeconds < 0 {
		return fmt.Errorf("MAX_LEASE_DURATION_SECONDS must not be negative")
	}
	if c.MaxLeaseDurationSeconds == 0 && IsRestrictedEnvironment(c.Env) {
		return fmt.Errorf("refusing to start in %s with MAX_LEASE_DURATION_SECONDS=0 (no lease ceiling)", c.Env)
	}
	return nil
}

// IsRestrictedEnvironment reports whether the deployment environment is one
// where the insecure fallbacks (plaintext master key, unauthenticated
// inbound) are refused by default.
func IsRestrictedEnvironment(env string) bool {
	e := strings.ToLower(strings.TrimSpace(env))
	return e == "production" || e == "staging"
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
