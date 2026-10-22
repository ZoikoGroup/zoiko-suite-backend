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
	// EgressPrivateCIDRs are private ranges a connector may point at because the provider is reached over
	// private connectivity (a dedicated link, a VPN, an on-premises gateway). An allowlist of exact ranges:
	// loopback, link-local (cloud metadata), unspecified and multicast stay refused whatever is listed.
	EgressPrivateCIDRs []string

	// SchemaRegistryURL turns on the producer-side contract gate (INT-CTRL-005): an event whose data does not satisfy its
	// registered contract is quarantined before any consumer sees it. Empty leaves the gate off.
	SchemaRegistryURL string
	// SchemaGatePrincipal is the workload identity the registry sees.
	SchemaGatePrincipal string
	// SchemaGateAllowUnregistered lets an event with no registered contract through. It exists to make a rollout
	// possible, not as a destination: while true, a producer with no contract is ungoverned.
	SchemaGateAllowUnregistered bool

	// Contract registry ceilings (ZS-SVC-T-001 section 19.1 leaves these open, so each is a knob with a safe default).
	// XICMaxPayloadBytes is OD-04, the largest schema or operation payload a contract may declare; the idempotency
	// window bounds are OD-06; XICCertificationMaxDays is OD-11, the longest a certification may stay valid.
	XICMaxPayloadBytes      int64
	XICIdempotencyMinDays   int
	XICIdempotencyMaxDays   int
	XICCertificationMaxDays int

	// XIC-02 connection policy. XICRedirectAllowlist is the exact set of pre-registered OAuth redirect targets (empty
	// means none can be used); the rest bound sessions, expiry warnings, rotation and break-glass.
	XICRedirectAllowlist     []string
	XICAuthSessionMinutes    int
	XICExpiryWarningDays     int
	XICRotationDays          int
	XICMaxGraceHours         int
	XICBreakGlassMaxMinutes  int
	XICEvaluatorIntervalSecs int // 0 turns the background evaluator off

	// XIC-03 inbound gateway. OFF by default: it opens an unauthenticated-by-ZoikoSuite endpoint for external senders.
	XICInboundEnabled      bool
	XICInboundMaxBodyBytes int64
	XICInboundTrustXFF     bool   // trust X-Forwarded-For for the per-address rate limit (only behind a proxy that sets it)
	XICEdgeSecret          string // shared with the TLS edge that attests client-certificate fingerprints
	XICRawRetentionDays    int    // OD-05; 0 keeps no raw bodies
	XICRawMaxBytes         int
	XICRawDenyClasses      []string
	XICHandoffIntervalSecs int // 0 turns the handoff worker off
	XICHandoffMaxAttempts  int
	XICConnRatePerMin      int
	XICIPRatePerMin        int

	// XIC-04 outbound engine.
	XICOutboundResolverIntervalSecs int // 0 turns the resolver worker off
	XICOutboundBodyRetentionHours   int // how long an OPEN attempt may keep its request body; 0 keeps none
	XICOutboundBodyMaxBytes         int
	XICOutboundBodyDenyClasses      []string
	XICOutboundRequestTimeoutSecs   int
	XICOutboundSubmitLeaseSecs      int
	XICOutboundResolveBackoffSecs   int
	XICOutboundMaxUnknownAgeHours   int // OD-13
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
		EgressPrivateCIDRs:      csv(os.Getenv("EGRESS_PRIVATE_CIDRS")),

		SchemaRegistryURL:           getEnv("SCHEMA_REGISTRY_URL", ""),
		SchemaGatePrincipal:         getEnv("SCHEMA_GATE_PRINCIPAL", "connectivity-api-bridge-svc"),
		SchemaGateAllowUnregistered: getEnv("SCHEMA_GATE_ALLOW_UNREGISTERED", "true") == "true",

		XICMaxPayloadBytes:              int64Env("XIC_MAX_PAYLOAD_BYTES", 25<<20),
		XICIdempotencyMinDays:           intEnv("XIC_IDEMPOTENCY_MIN_DAYS", 1),
		XICIdempotencyMaxDays:           intEnv("XIC_IDEMPOTENCY_MAX_DAYS", 3650),
		XICCertificationMaxDays:         intEnv("XIC_CERTIFICATION_MAX_DAYS", 365),
		XICRedirectAllowlist:            exactCSV(os.Getenv("XIC_REDIRECT_ALLOWLIST")),
		XICAuthSessionMinutes:           intEnv("XIC_AUTH_SESSION_MINUTES", 10),
		XICExpiryWarningDays:            intEnv("XIC_EXPIRY_WARNING_DAYS", 14),
		XICRotationDays:                 intEnv("XIC_ROTATION_DAYS", 90),
		XICMaxGraceHours:                intEnv("XIC_MAX_GRACE_HOURS", 168),
		XICBreakGlassMaxMinutes:         intEnv("XIC_BREAK_GLASS_MAX_MINUTES", 240),
		XICEvaluatorIntervalSecs:        nonNegInt("XIC_EVALUATOR_INTERVAL_SECONDS", 300),
		XICInboundEnabled:               getEnv("XIC_INBOUND_ENABLED", "false") == "true",
		XICInboundMaxBodyBytes:          int64Env("XIC_INBOUND_MAX_BODY_BYTES", 5<<20),
		XICInboundTrustXFF:              getEnv("XIC_INBOUND_TRUST_XFF", "false") == "true",
		XICEdgeSecret:                   os.Getenv("XIC_EDGE_SECRET"),
		XICRawRetentionDays:             nonNegInt("XIC_RAW_RETENTION_DAYS", 30),
		XICRawMaxBytes:                  intEnv("XIC_RAW_MAX_BYTES", 1<<20),
		XICRawDenyClasses:               upperCSV(getEnv("XIC_RAW_DENY_CLASSES", "PERSONAL,RESTRICTED")),
		XICHandoffIntervalSecs:          nonNegInt("XIC_HANDOFF_INTERVAL_SECONDS", 5),
		XICHandoffMaxAttempts:           intEnv("XIC_HANDOFF_MAX_ATTEMPTS", 8),
		XICConnRatePerMin:               nonNegInt("XIC_INBOUND_CONN_RATE_PER_MIN", 600),
		XICIPRatePerMin:                 nonNegInt("XIC_INBOUND_IP_RATE_PER_MIN", 1200),
		XICOutboundResolverIntervalSecs: nonNegInt("XIC_OUTBOUND_RESOLVER_INTERVAL_SECONDS", 15),
		XICOutboundBodyRetentionHours:   nonNegInt("XIC_OUTBOUND_BODY_RETENTION_HOURS", 168),
		XICOutboundBodyMaxBytes:         intEnv("XIC_OUTBOUND_BODY_MAX_BYTES", 256<<10),
		XICOutboundBodyDenyClasses:      upperCSV(getEnv("XIC_OUTBOUND_BODY_DENY_CLASSES", "PERSONAL,RESTRICTED")),
		XICOutboundRequestTimeoutSecs:   intEnv("XIC_OUTBOUND_REQUEST_TIMEOUT_SECONDS", 20),
		XICOutboundSubmitLeaseSecs:      intEnv("XIC_OUTBOUND_SUBMIT_LEASE_SECONDS", 120),
		XICOutboundResolveBackoffSecs:   intEnv("XIC_OUTBOUND_RESOLVE_BACKOFF_SECONDS", 30),
		XICOutboundMaxUnknownAgeHours:   intEnv("XIC_OUTBOUND_MAX_UNKNOWN_AGE_HOURS", 24),
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

// intEnv reads a positive integer, falling back when the variable is unset or not one: a typo in a ceiling must
// not become zero, which would make every contract invalid.
func intEnv(key string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func int64Env(key string, fallback int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

// exactCSV splits a comma-separated list WITHOUT changing case: a redirect target is matched exactly.
func exactCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nonNegInt reads an integer that may be zero (to switch something off).
func nonNegInt(key string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func upperCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.ToUpper(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}
