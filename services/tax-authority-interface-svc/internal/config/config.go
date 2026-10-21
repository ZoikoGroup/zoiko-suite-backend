package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port             string
	DatabaseURL      string
	KafkaBrokers     string
	KafkaEventsTopic string
	AuthzServiceURL  string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// EventResidencyRegion is the residency region every event this service enqueues to the transactional
	// outbox carries (ZS-EVENT-001 section 4, required): a lower-case code such as "uk" or "eu-west".
	EventResidencyRegion string

	// Authority adapter (ZS-SVC-M-001 INT-08). Off by default: it needs a connector service to resolve approved
	// connectors, a vault to release credentials, and an explicit egress policy.
	Authority AuthorityConfig
	// CallbacksJSON lists the authorities this deployment accepts callbacks from and how each is verified and
	// read; secrets come from the vault, per tenant, for the one verification.
	CallbacksJSON string
}

// AuthorityConfig configures the live tax-authority adapter.
type AuthorityConfig struct {
	Enabled         bool
	ConnectivityURL string // connectivity-api-bridge-svc
	VaultURL        string // secret-vault-integration-svc
	WorkloadID      string // this service's identity at the vault; the secret policies must allow it
	// AllowHTTP is for local development only.
	AllowHTTP    bool
	AllowedPorts []int
	// PrivateCIDRs are private ranges an authority may be reached on (dedicated links, VPN, gateways).
	PrivateCIDRs       []string
	StaleAfter         time.Duration
	SweepInterval      time.Duration
	PollTick           time.Duration // how often the worker looks for filings whose poll time has come
	PollInterval       time.Duration // between polls of a filing the authority holds
	UrgentPollInterval time.Duration // between polls once the filing deadline is at risk
	BreakerWindow      time.Duration
	BreakerFailures    int
	MaxResponse        int64
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8147"),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/zoiko_tax_authority_interface?sslmode=disable"),
		KafkaBrokers:     getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic: getEnv("KAFKA_EVENTS_TOPIC", "zoiko.tax.authority.interface.events"),
		AuthzServiceURL:  getEnv("AUTHZ_SERVICE_URL", "http://localhost:8081"),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		EventResidencyRegion: getEnv("EVENT_RESIDENCY_REGION", "uk"),
		CallbacksJSON:        os.Getenv("AUTHORITY_CALLBACKS_JSON"),
		Authority: AuthorityConfig{
			Enabled:            os.Getenv("AUTHORITY_ADAPTER_ENABLED") == "true",
			ConnectivityURL:    getEnv("CONNECTIVITY_SERVICE_URL", "http://connectivity-api-bridge-svc:8144"),
			VaultURL:           getEnv("VAULT_SERVICE_URL", "http://secret-vault-integration-svc:8087"),
			WorkloadID:         getEnv("VAULT_WORKLOAD_ID", "tax-authority-interface-svc"),
			AllowHTTP:          os.Getenv("AUTHORITY_ALLOW_HTTP") == "true",
			AllowedPorts:       ports(os.Getenv("AUTHORITY_ALLOWED_PORTS")),
			PrivateCIDRs:       csv(os.Getenv("AUTHORITY_PRIVATE_CIDRS")),
			StaleAfter:         envDuration("AUTHORITY_STALE_AFTER", 2*time.Minute),
			SweepInterval:      envDuration("AUTHORITY_SWEEP_INTERVAL", 30*time.Second),
			PollTick:           envDuration("AUTHORITY_POLL_TICK", 10*time.Second),
			PollInterval:       envDuration("AUTHORITY_POLL_INTERVAL", time.Minute),
			UrgentPollInterval: envDuration("AUTHORITY_URGENT_POLL_INTERVAL", 15*time.Second),
			BreakerWindow:      envDuration("AUTHORITY_BREAKER_WINDOW", time.Minute),
			BreakerFailures:    envInt("AUTHORITY_BREAKER_FAILURES", 5),
			MaxResponse:        int64(envInt("AUTHORITY_MAX_RESPONSE_BYTES", 256<<10)),
		},
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func csv(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ports(v string) []int {
	var out []int
	for _, p := range csv(v) {
		if n, err := strconv.Atoi(p); err == nil && n >= 1 && n <= 65535 {
			out = append(out, n)
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
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}
