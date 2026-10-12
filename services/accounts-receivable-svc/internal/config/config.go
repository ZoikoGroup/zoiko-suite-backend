package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for accounts-receivable-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthZServiceURL is the base URL of authorization-svc. Gated transitions
	// are checked synchronously.
	AuthZServiceURL string

	// TenantRegistryURL is the base URL of tenant-entity-registry-svc. Every write
	// reconciles its legal_entity_id against the caller's verified tenant there,
	// and fails closed if it cannot — see internal/entity.
	TenantRegistryURL string
	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthZServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// LedgerServiceURL is the base URL of general-ledger-svc. We verify
	// finalized journal existence before allowing SENT -> PAID or OVERDUE -> PAID.
	LedgerServiceURL string

	// ── Downstream contract services used by the AR-01..AR-10 modules ──────

	// TaxDeterminationURL is tax-determination-svc (TAX-03): AR never computes tax.
	TaxDeterminationURL string
	// WorkflowURL is workflow-svc: invoice / correction / write-off approvals.
	WorkflowURL string
	// DelegatedAuthorityURL is delegated-authority-svc: approval limits.
	DelegatedAuthorityURL string
	// NotificationURL is notification-svc: invoice delivery and dunning notices.
	NotificationURL string
	// BankingConnectorURL is banking-connector-svc: Banking-confirmed receipts (AR-08).
	BankingConnectorURL string
	// PaymentInitiationURL / PaymentStatusURL are the outbound payment path for refunds (AR-10).
	PaymentInitiationURL string
	PaymentStatusURL     string
	// JurisdictionRulesURL is jurisdiction-rules-svc: e-invoice profiles and submission registry (AR-05).
	JurisdictionRulesURL string
	// AccountingPeriodURL is accounting-period-svc: period state gate before issue/posting.
	AccountingPeriodURL string
	// PostingPrincipalID is the service identity granted GL_POSTING_EXECUTE in
	// general-ledger-svc. Empty = post as the principal who caused the posting.
	PostingPrincipalID string

	// CounterpartyMgmtURL is counterparty-management-svc: the customer party
	// master that AR-01 validates a customer against.
	CounterpartyMgmtURL string

	// OTELExporterEndpoint is where telemetry sends trace data.
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

// KafkaConfig mirrors every other producer in this platform.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8101),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "accounts_receivable"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "accounts-receivable-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.accounts-receivable.events"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		LedgerServiceURL:     env("LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		TenantRegistryURL:    env("TENANT_REGISTRY_URL", "http://tenant-entity-registry-svc:8081"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		TaxDeterminationURL:   env("TAX_DETERMINATION_URL", "http://tax-determination-svc:8126"),
		WorkflowURL:           env("WORKFLOW_SERVICE_URL", "http://workflow-svc:8090"),
		DelegatedAuthorityURL: env("DELEGATED_AUTHORITY_URL", "http://delegated-authority-svc:8136"),
		NotificationURL:       env("NOTIFICATION_SERVICE_URL", "http://notification-svc:8133"),
		BankingConnectorURL:   env("BANKING_CONNECTOR_URL", "http://banking-connector-svc:8145"),
		PaymentInitiationURL:  env("PAYMENT_INITIATION_URL", "http://payment-initiation-adapter-svc:8162"),
		PaymentStatusURL:      env("PAYMENT_STATUS_URL", "http://payment-status-svc:8163"),
		JurisdictionRulesURL:  env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8082"),
		AccountingPeriodURL:   env("ACCOUNTING_PERIOD_URL", "http://accounting-period-svc:8174"),
		PostingPrincipalID:    env("AR_POSTING_PRINCIPAL_ID", ""),
		CounterpartyMgmtURL:   env("COUNTERPARTY_MGMT_URL", "http://counterparty-management-svc:8124"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
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
