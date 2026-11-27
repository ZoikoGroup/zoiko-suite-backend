package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config holds all runtime configuration for general-ledger-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// EventResidencyRegion is the residencyregion every event this service
	// emits carries (ZS-EVENT-001 §4: required, "authoritative regional
	// data-bearing context"). Deliberately NO default: a wrong region silently
	// stamped on every financial event is worse than refusing to start.
	// EVENT_RESIDENCY_REGION, a lower-case code such as "uk" or "eu-west".
	EventResidencyRegion string

	// AuthZServiceURL is the base URL of authorization-svc. Every mutating
	// journal action (create/validate/post/reverse) is checked against it
	// synchronously before applying — no service self-authorizes (doctrine,
	// 03-microservices.md). Fail-closed: unreachable authorization-svc
	// rejects the action, see internal/authz.HTTPClient.
	AuthZServiceURL string

	// AuthzMTLSEnabled turns on the mTLS pilot for calls to authorization-svc.
	// OFF by default — when false, nothing about the existing authz call
	// changes.
	AuthzMTLSEnabled bool
	// AuthzMTLSURL is authorization-svc's mTLS listener, used only when
	// AuthzMTLSEnabled is true.
	AuthzMTLSURL string
	// MTLSManagementServiceURL is where this service provisions its own
	// client-side mTLS identity, used only when AuthzMTLSEnabled is true.
	MTLSManagementServiceURL string

	// CloseServiceURL is the base URL of financial-close-svc.
	CloseServiceURL string

	// EvidenceRequirementsURL is the base URL of evidence-requirements-svc.
	// Posting is a finalization path and must not skip required evidence.
	EvidenceRequirementsURL string

	// DataQualityServiceURL and ReconciliationServiceURL are the services that issue the certifications a journal's evidence may
	// cite (evidence reference "certification:<service>:<run id>"). A posted journal records its reliance with the issuing service
	// so a later revocation can name it (ZS-SVC-Z-001 RB-05, DQC-017). A journal that cites no certification never calls them.
	DataQualityServiceURL    string
	ReconciliationServiceURL string

	// CertSweepPrincipalID (CERT_SWEEP_PRINCIPAL_ID) is the principal the scheduled certification sweep acts as, and CertSweepInterval
	// (CERT_SWEEP_INTERVAL, default 15m, "off" to disable) how often it runs. Without a principal the scheduler does not start: there is no
	// default identity, because the principal must be GRANTED GL_CERTIFICATION_WATCH_SWEEP on the entities it sweeps and read on the
	// certifications, like anyone else (no service self-authorizes).
	CertSweepPrincipalID string
	CertSweepInterval    time.Duration

	// The event-driven trigger (certwatch.Consumer): CertWithdrawalConsumer (CERT_WITHDRAWAL_CONSUMER, "on" by default, "off" to disable),
	// the topics it reads (CERT_WITHDRAWAL_TOPICS, comma separated; default both issuing services' event topics) and its consumer group
	// (CERT_WITHDRAWAL_GROUP_ID). It starts only when CertSweepPrincipalID is set, because it acts as the same principal as the schedule.
	CertWithdrawalConsumer bool
	CertWithdrawalTopics   []string
	CertWithdrawalGroupID  string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// PeriodGateMode is the REF-05 posting-gate mode: "off" (default) or
	// "shadow" (consult + compare only; never changes any outcome). "enforce"
	// is NOT implemented in this phase and is rejected at startup.
	PeriodGateMode string
	// AccountingPeriodURL is accounting-period-svc (REF-05).
	AccountingPeriodURL string

	// CurrencyRegistryURL is currency-registry-svc (REF-02): the accounting kernel reads currency precision from it.
	CurrencyRegistryURL string
	// ManualJournalEvidenceThreshold (MANUAL_JOURNAL_EVIDENCE_THRESHOLD, e.g. "10000.00") is the total above which a manual
	// journal must carry evidence before it can be submitted; empty or 0 disables the rule. ManualJournalEvidenceTypes
	// (MANUAL_JOURNAL_EVIDENCE_TYPES, e.g. "OPENING,CLOSING") are the journal types that always need evidence.
	ManualJournalEvidenceThreshold string
	ManualJournalEvidenceTypes     string
	// SnapshotSigningKey (SNAPSHOT_SIGNING_KEY, at least 32 characters) signs retained balance snapshots; SnapshotSigningKeyID
	// (SNAPSHOT_SIGNING_KEY_ID, default "k1") names it so a later rotation can still verify older snapshots. Without a key the
	// snapshot command refuses.
	SnapshotSigningKey   string
	SnapshotSigningKeyID string
	// PeriodGateShadowTimeout bounds each shadow resolve call.
	PeriodGateShadowTimeout time.Duration
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
	mode, err := ParsePeriodGateMode(os.Getenv("PERIOD_GATE_MODE"))
	if err != nil {
		return nil, err
	}
	sweepInterval, err := ParseCertSweepInterval(os.Getenv("CERT_SWEEP_INTERVAL"))
	if err != nil {
		return nil, err
	}
	withdrawalOn, err := ParseOnOff("CERT_WITHDRAWAL_CONSUMER", os.Getenv("CERT_WITHDRAWAL_CONSUMER"), true)
	if err != nil {
		return nil, err
	}
	withdrawalTopics, err := ParseTopics(os.Getenv("CERT_WITHDRAWAL_TOPICS"))
	if err != nil {
		return nil, err
	}
	timeout := 300 * time.Millisecond
	if v := strings.TrimSpace(os.Getenv("PERIOD_GATE_SHADOW_TIMEOUT")); v != "" {
		d, perr := time.ParseDuration(v)
		if perr != nil || d <= 0 {
			return nil, fmt.Errorf("PERIOD_GATE_SHADOW_TIMEOUT %q must be a positive duration such as 300ms", v)
		}
		timeout = d
	}
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if region == "" {
		return nil, errors.New("EVENT_RESIDENCY_REGION is required (e.g. uk, eu-west): every emitted event must carry its residency region")
	}
	return &Config{
		EventResidencyRegion: region,
		Env:                  env("ENV", "local"),
		// 8098: 8080-8097 are already taken by every other service built so
		// far (search-indexer-svc claimed 8096, workflow-history-svc claimed
		// 8097 on a sibling in-flight branch) — see services/README.md.
		Port: envInt("PORT", 8098),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "general_ledger"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "general-ledger-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.general-ledger.events"),
		},
		AuthZServiceURL:                env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AuthzMTLSEnabled:               os.Getenv("AUTHZ_MTLS_ENABLED") == "true",
		AuthzMTLSURL:                   env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL:       env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		CloseServiceURL:                env("CLOSE_SERVICE_URL", "http://financial-close-svc:8104"),
		EvidenceRequirementsURL:        env("EVIDENCE_REQUIREMENTS_URL", "http://evidence-requirements-svc:8130"),
		DataQualityServiceURL:          env("DATA_QUALITY_SERVICE_URL", "http://data-quality-svc:8178"),
		ReconciliationServiceURL:       env("RECONCILIATION_SERVICE_URL", "http://reconciliation-engine-svc:8182"),
		OTELExporterEndpoint:           env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		PeriodGateMode:                 mode,
		AccountingPeriodURL:            env("ACCOUNTING_PERIOD_URL", "http://accounting-period-svc:8174"),
		CurrencyRegistryURL:            env("CURRENCY_REGISTRY_URL", "http://currency-registry-svc:8172"),
		ManualJournalEvidenceThreshold: env("MANUAL_JOURNAL_EVIDENCE_THRESHOLD", ""),
		ManualJournalEvidenceTypes:     env("MANUAL_JOURNAL_EVIDENCE_TYPES", ""),
		SnapshotSigningKey:             env("SNAPSHOT_SIGNING_KEY", ""),
		SnapshotSigningKeyID:           env("SNAPSHOT_SIGNING_KEY_ID", "k1"),
		PeriodGateShadowTimeout:        timeout,
		CertSweepPrincipalID:           strings.TrimSpace(os.Getenv("CERT_SWEEP_PRINCIPAL_ID")),
		CertSweepInterval:              sweepInterval,
		CertWithdrawalConsumer:         withdrawalOn,
		CertWithdrawalTopics:           withdrawalTopics,
		CertWithdrawalGroupID:          env("CERT_WITHDRAWAL_GROUP_ID", "general-ledger-svc.certification-withdrawals"),
	}, nil
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

// ParsePeriodGateMode validates PERIOD_GATE_MODE. Only "off" (also when unset)
// and "shadow" are accepted; "enforce" gets a specific error because it is the
// obvious next value and silently accepting it would make operators believe
// the gate is enforcing when it is not.
func ParsePeriodGateMode(raw string) (string, error) {
	switch m := strings.ToLower(strings.TrimSpace(raw)); m {
	case "", "off":
		return "off", nil
	case "shadow":
		return "shadow", nil
	case "enforce":
		return "", fmt.Errorf("PERIOD_GATE_MODE=enforce is not implemented in this phase: enforcement is phase 4 and requires separate sign-off; allowed values are off or shadow")
	default:
		return "", fmt.Errorf("invalid PERIOD_GATE_MODE %q: allowed values are off or shadow (enforce is not implemented in this phase)", raw)
	}
}

const (
	defaultCertSweepInterval = 15 * time.Minute
	minCertSweepInterval     = time.Minute
	maxCertSweepInterval     = 24 * time.Hour
)

// ParseCertSweepInterval validates CERT_SWEEP_INTERVAL: unset means 15 minutes, "off" (or 0) disables the schedule, and anything
// else is a duration between one minute and 24 hours. A sweep calls other services for every watched certification, so a tighter
// bound than a minute is refused, and one longer than a day would leave a revocation unfound for longer than the sweep is worth.
func ParseCertSweepInterval(raw string) (time.Duration, error) {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		return defaultCertSweepInterval, nil
	case "off", "0", "0s":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < minCertSweepInterval || d > maxCertSweepInterval {
		return 0, fmt.Errorf("CERT_SWEEP_INTERVAL %q must be \"off\" or a duration between %s and %s, such as 15m", raw, minCertSweepInterval, maxCertSweepInterval)
	}
	return d, nil
}

// ParseOnOff validates a switch such as CERT_WITHDRAWAL_CONSUMER: unset means def, and only on/off (any case) is accepted, so a typo
// such as "of" stops the service at startup instead of silently leaving the feature in a state nobody chose.
func ParseOnOff(name, raw string, def bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return def, nil
	case "on", "true":
		return true, nil
	case "off", "false":
		return false, nil
	}
	return false, fmt.Errorf("%s %q must be on or off", name, raw)
}

var topicNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,248}$`)

// DefaultCertWithdrawalTopics are the event topics of the two services that issue certifications.
var DefaultCertWithdrawalTopics = []string{"zoiko.data.quality.events", "zoiko.data.reconciliation_engine.events"}

// ParseTopics validates CERT_WITHDRAWAL_TOPICS: unset means both issuing services' topics, otherwise a comma separated list of valid,
// distinct topic names.
func ParseTopics(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), DefaultCertWithdrawalTopics...), nil
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if !topicNameRe.MatchString(t) {
			return nil, fmt.Errorf("CERT_WITHDRAWAL_TOPICS: %q is not a valid topic name", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}
