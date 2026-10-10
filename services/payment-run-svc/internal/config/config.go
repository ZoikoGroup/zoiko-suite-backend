package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                           string
	DatabaseURL                    string
	KafkaBrokers                   string
	KafkaEventsTopic               string
	AuthzServiceURL                string
	PaymentAuthorizationServiceURL string
	PaymentInitiationAdapterURL    string
	PaymentStatusServiceURL        string
	PaymentProposalServiceURL      string
	PayableOpenItemServiceURL      string

	// ACC-04 settlement posting. LedgerServiceURL is general-ledger-svc.
	// AccountingPrincipalID is the service identity sent as X-Principal-Id and
	// must hold the ledger's GL_POSTING_EXECUTE action; when it is empty the
	// dispatcher is not started and requests simply stay PENDING (nothing is
	// lost, nothing is posted under an invented identity).
	LedgerServiceURL      string
	AccountingPrincipalID string
	// ACC-02 mapping keys. The ledger resolves them to accounts.
	PayableControlKey     string
	PaymentClearingKey    string
	WithholdingPayableKey string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8161"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                           port,
		DatabaseURL:                    dbURL,
		KafkaBrokers:                   getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:               getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-run.events"),
		AuthzServiceURL:                getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		PaymentAuthorizationServiceURL: getEnvOrDefault("PAYMENT_AUTHORIZATION_SERVICE_URL", "http://payment-authorization-svc:8160"),
		PaymentInitiationAdapterURL:    getEnvOrDefault("PAYMENT_INITIATION_ADAPTER_URL", "http://payment-initiation-adapter-svc:8162"),
		PaymentStatusServiceURL:        getEnvOrDefault("PAYMENT_STATUS_SERVICE_URL", "http://payment-status-svc:8163"),
		PaymentProposalServiceURL:      getEnvOrDefault("PAYMENT_PROPOSAL_SERVICE_URL", "http://payment-proposal-svc:8159"),
		PayableOpenItemServiceURL:      getEnvOrDefault("PAYABLE_OPEN_ITEM_SERVICE_URL", "http://payable-open-item-svc:8164"),
		LedgerServiceURL:               getEnvOrDefault("LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		AccountingPrincipalID:          os.Getenv("ACCOUNTING_PRINCIPAL_ID"),
		PayableControlKey:              getEnvOrDefault("AP_PAYABLE_CONTROL_KEY", "AP_PAYABLE_CONTROL"),
		PaymentClearingKey:             getEnvOrDefault("AP_PAYMENT_CLEARING_KEY", "AP_PAYMENT_CLEARING"),
		WithholdingPayableKey:          getEnvOrDefault("AP_WITHHOLDING_PAYABLE_KEY", "AP_WITHHOLDING_PAYABLE"),
	}, nil
}

func (c *Config) DSN() string {
	return c.DatabaseURL
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
