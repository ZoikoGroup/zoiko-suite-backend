package config

import (
	"os"
	"strconv"
	"strings"
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

	// EventResidencyRegion is the residency region every event this service
	// enqueues to the transactional outbox carries (ZS-EVENT-001 §4, required):
	// EVENT_RESIDENCY_REGION, a lower-case code such as "uk" or "eu-west".
	EventResidencyRegion string

	// VaultServiceURL is secret-vault-integration-svc. A connector's credential
	// reference is confirmed against it when the connector is submitted,
	// approved and activated. VaultMTLSEnabled/VaultMTLSURL switch that call to
	// the mTLS listener, using the same workload identity as authorization-svc.
	VaultServiceURL  string
	VaultMTLSEnabled bool
	VaultMTLSURL     string

	// EgressAllowedRegions are the regions this deployment may send connector
	// data to (INT-CTRL-015). EMPTY MEANS NONE: with nothing configured no
	// connector can be submitted or approved. That is deliberate; an
	// unconfigured allowlist must not behave like an open one.
	EgressAllowedRegions []string
	// EgressAllowInsecureHTTP permits http:// destinations. Development only.
	EgressAllowInsecureHTTP bool
	// EgressAllowedPorts are the destination ports a connector may use. Empty
	// means 443 only.
	EgressAllowedPorts []int
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8144"),
		DatabaseURL:      getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/zoiko_connectivity_api_bridge?sslmode=disable"),
		KafkaBrokers:     getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic: getEnv("KAFKA_EVENTS_TOPIC", "zoiko.connectivity.api.bridge.events"),
		AuthzServiceURL:  getEnv("AUTHZ_SERVICE_URL", "http://localhost:8081"),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		EventResidencyRegion: getEnv("EVENT_RESIDENCY_REGION", "uk"),

		VaultServiceURL:  getEnv("SECRET_VAULT_URL", "http://secret-vault-integration-svc:8087"),
		VaultMTLSEnabled: getEnv("VAULT_MTLS_ENABLED", "false") == "true",
		VaultMTLSURL:     getEnv("VAULT_MTLS_URL", "https://secret-vault-integration-svc:8449"),

		EgressAllowedRegions:    csv(os.Getenv("EGRESS_ALLOWED_REGIONS")),
		EgressAllowInsecureHTTP: getEnv("EGRESS_ALLOW_INSECURE_HTTP", "false") == "true",
		EgressAllowedPorts:      ints(os.Getenv("EGRESS_ALLOWED_PORTS")),
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
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ints parses a comma-separated port list, ignoring anything that is not a
// valid port.
func ints(v string) []int {
	var out []int
	for _, p := range strings.Split(v, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil && n >= 1 && n <= 65535 {
			out = append(out, n)
		}
	}
	return out
}
