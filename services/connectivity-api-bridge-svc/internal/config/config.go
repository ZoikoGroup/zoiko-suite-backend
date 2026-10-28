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

	// XIC-04 provider callbacks. OFF by default: the endpoint is reachable by external systems.
	XICCallbacksEnabled       bool
	XICCallbackMaxBodyBytes   int64
	XICCallbackConnRatePerMin int
	XICCallbackIPRatePerMin   int
	XICCallbackInvalidPerMin  int

	// XIC-05 batch and file exchange. OFF by default: the HTTPS object channel is reachable by external systems.
	XICBatchEnabled              bool
	XICBatchMaxRequestBytes      int64
	XICBatchMaxFileBytes         int64
	XICBatchMaxDecompressedBytes int64
	XICBatchContentRetentionHrs  int
	XICBatchProcessingGraceMins  int
	XICBatchContentDenyClasses   []string
	XICBatchConnRatePerMin       int
	XICBatchIPRatePerMin         int
	XICBatchWorkerIntervalSecs   int // 0 turns the batch worker off
	XICBatchRowChunk             int
	XICReplayMaxItems            int
	XICReplayApprovalRows        int
	XICReplayWindowMaxHours      int
	XICReplayRegulatedClasses    []string
	XICBatchClamAVAddr           string // a malware scanner is required unless built-in checks only are accepted
	XICBatchAllowBuiltinScanOnly bool
	XICBatchScanTimeoutSecs      int
	XICSFTPPollIntervalSecs      int // 0 turns the SFTP pull poller off
	XICSFTPInbox                 string
	XICSFTPDone                  string
	XICSFTPDefaultPort           int

	// XIC-05 reconciliation and signals.
	XICReconWorkerIntervalSecs int // 0 turns the reconciliation worker and scheduler off
	XICReconScheduleEverySecs  int
	XICReconMaxPeriodHours     int
	XICReconPoliciesJSON       string // optional per-family overrides (OD-12)
	XICMetricsToken            string // /metrics is mounted only when a token is configured
	XICMetricsWindowHours      int

	// Wave 8: staged rollout and legacy migration.
	XICRolloutRequired        bool // a production connection with no rollout carries nothing
	XICRolloutWorkerSecs      int  // 0 turns the pilot evaluator off
	XICRolloutRequestTTLHours int
	XICUnknownSLAHours        int // OD-13: an UNKNOWN attempt older than this blocks a step forward and rolls a pilot back
	XICParityMinSamples       int
	XICParityMaxAgeHours      int
	XICShadowMinHours         int
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
		XICCallbacksEnabled:             getEnv("XIC_CALLBACKS_ENABLED", "false") == "true",
		XICCallbackMaxBodyBytes:         int64Env("XIC_CALLBACK_MAX_BODY_BYTES", 1<<20),
		XICCallbackConnRatePerMin:       nonNegInt("XIC_CALLBACK_CONN_RATE_PER_MIN", 600),
		XICCallbackIPRatePerMin:         nonNegInt("XIC_CALLBACK_IP_RATE_PER_MIN", 1200),
		XICCallbackInvalidPerMin:        nonNegInt("XIC_CALLBACK_INVALID_EVIDENCE_PER_MIN", 30),
		XICBatchEnabled:                 getEnv("XIC_BATCH_ENABLED", "false") == "true",
		XICBatchMaxRequestBytes:         int64Env("XIC_BATCH_MAX_REQUEST_BYTES", 64<<20),
		XICBatchMaxFileBytes:            int64Env("XIC_BATCH_MAX_FILE_BYTES", 25<<20),
		XICBatchMaxDecompressedBytes:    int64Env("XIC_BATCH_MAX_DECOMPRESSED_BYTES", 256<<20),
		XICBatchContentRetentionHrs:     nonNegInt("XIC_BATCH_CONTENT_RETENTION_HOURS", 168),
		XICBatchProcessingGraceMins:     nonNegInt("XIC_BATCH_PROCESSING_GRACE_MINUTES", 60),
		XICBatchContentDenyClasses:      upperCSVDefault("XIC_BATCH_CONTENT_DENY_CLASSES", "PERSONAL,RESTRICTED"),
		XICBatchConnRatePerMin:          nonNegInt("XIC_BATCH_CONN_RATE_PER_MIN", 60),
		XICBatchIPRatePerMin:            nonNegInt("XIC_BATCH_IP_RATE_PER_MIN", 120),
		XICBatchWorkerIntervalSecs:      nonNegInt("XIC_BATCH_WORKER_INTERVAL_SECS", 5),
		XICBatchRowChunk:                nonNegInt("XIC_BATCH_ROW_CHUNK", 100),
		XICReplayMaxItems:               nonNegInt("XIC_REPLAY_MAX_ITEMS", 50000),
		XICReplayApprovalRows:           nonNegInt("XIC_REPLAY_APPROVAL_ROWS", 100),
		XICReplayWindowMaxHours:         nonNegInt("XIC_REPLAY_WINDOW_MAX_HOURS", 168),
		XICReplayRegulatedClasses:       upperCSVDefault("XIC_REPLAY_REGULATED_CLASSES", "REGULATED,PAYROLL,TAX"),
		XICBatchClamAVAddr:              os.Getenv("XIC_BATCH_CLAMAV_ADDR"),
		XICBatchAllowBuiltinScanOnly:    getEnv("XIC_BATCH_ALLOW_BUILTIN_SCAN_ONLY", "false") == "true",
		XICBatchScanTimeoutSecs:         nonNegInt("XIC_BATCH_SCAN_TIMEOUT_SECS", 30),
		XICSFTPPollIntervalSecs:         nonNegInt("XIC_SFTP_POLL_INTERVAL_SECS", 60),
		XICSFTPInbox:                    getEnv("XIC_SFTP_INBOX", "inbox"),
		XICSFTPDone:                     getEnv("XIC_SFTP_DONE", "inbox/processed"),
		XICSFTPDefaultPort:              nonNegInt("XIC_SFTP_DEFAULT_PORT", 22),
		XICReconWorkerIntervalSecs:      nonNegInt("XIC_RECON_WORKER_INTERVAL_SECS", 10),
		XICReconScheduleEverySecs:       nonNegInt("XIC_RECON_SCHEDULE_EVERY_SECS", 60),
		XICReconMaxPeriodHours:          nonNegInt("XIC_RECON_MAX_PERIOD_HOURS", 744),
		XICReconPoliciesJSON:            os.Getenv("XIC_RECON_POLICIES_JSON"),
		XICMetricsToken:                 os.Getenv("XIC_METRICS_TOKEN"),
		XICMetricsWindowHours:           nonNegInt("XIC_METRICS_WINDOW_HOURS", 24),
		XICRolloutRequired:              getEnv("XIC_ROLLOUT_REQUIRED", "false") == "true",
		XICRolloutWorkerSecs:            nonNegInt("XIC_ROLLOUT_WORKER_INTERVAL_SECS", 30),
		XICRolloutRequestTTLHours:       nonNegInt("XIC_ROLLOUT_REQUEST_TTL_HOURS", 72),
		XICUnknownSLAHours:              nonNegInt("XIC_UNKNOWN_SLA_HOURS", 24),
		XICParityMinSamples:             nonNegInt("XIC_PARITY_MIN_SAMPLES", 20),
		XICParityMaxAgeHours:            nonNegInt("XIC_PARITY_MAX_AGE_HOURS", 24),
		XICShadowMinHours:               nonNegInt("XIC_SHADOW_MIN_HOURS", 24),
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

// upperCSVDefault reads a comma list in upper case, or the default when the variable is unset. An empty value set on
// purpose means an empty list.
func upperCSVDefault(key, def string) []string {
	v, ok := os.LookupEnv(key)
	if !ok {
		v = def
	}
	return upperCSV(v)
}
