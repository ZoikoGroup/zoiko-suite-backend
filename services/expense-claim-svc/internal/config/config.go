package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                       string
	DatabaseURL                string
	KafkaBrokers               string
	KafkaEventsTopic           string
	AuthzServiceURL            string
	EmployeeMasterServiceURL   string
	DocumentVaultServiceURL    string
	TaxDeterminationServiceURL string
	PolicyServiceURL           string
	PayableOpenItemServiceURL  string
	PayeeIdentityServiceURL    string
	ReceiptRequiredThreshold   float64

	ConfigFeatureFlagServiceURL string
	Environment                 string

	// ReimbursementTermsDays is the default payment term, in days after
	// approval, of the payable an approved claim creates (overridable per tenant
	// via configuration-feature-flag-svc EXPENSE_REIMBURSEMENT_TERMS_DAYS).
	ReimbursementTermsDays float64
	// PolicyControlledCategories are the expense categories for which a missing
	// or unavailable approval-threshold policy blocks submission ("*" = every
	// category).
	PolicyControlledCategories []string

	// ACC-04 posting. GeneralLedgerServiceURL is general-ledger-svc.
	// AccountingPrincipalID is the service identity sent as X-Principal-Id and
	// must hold the ledger's GL_POSTING_EXECUTE action; when it is empty the
	// dispatcher is not started and posting requests stay PENDING (nothing is
	// lost, nothing is posted under an invented identity).
	GeneralLedgerServiceURL string
	AccountingPrincipalID   string
	// ACC-02 mapping keys the posting uses (the ledger resolves them to
	// accounts; this service never names a GL account) and the fiscal-period
	// layout derived from the document date.
	PostingExpenseKey        string
	PostingPayableKey        string
	PostingTaxRecoverableKey string
	FiscalPeriodLayout       string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8158"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                       port,
		DatabaseURL:                dbURL,
		KafkaBrokers:               getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:           getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.expense-claim.events"),
		AuthzServiceURL:            getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		EmployeeMasterServiceURL:   getEnvOrDefault("EMPLOYEE_MASTER_SERVICE_URL", "http://employee-master-svc:8108"),
		DocumentVaultServiceURL:    getEnvOrDefault("DOCUMENT_VAULT_SERVICE_URL", "http://document-vault-svc:8094"),
		TaxDeterminationServiceURL: getEnvOrDefault("TAX_DETERMINATION_SERVICE_URL", "http://tax-determination-svc:8126"),
		PolicyServiceURL:           getEnvOrDefault("POLICY_SERVICE_URL", "http://policy-svc:8085"),
		PayableOpenItemServiceURL:  getEnvOrDefault("PAYABLE_OPEN_ITEM_SERVICE_URL", "http://payable-open-item-svc:8164"),
		PayeeIdentityServiceURL:    getEnvOrDefault("PAYEE_IDENTITY_SERVICE_URL", "http://payee-banking-identity-svc:8166"),
		ReceiptRequiredThreshold:   getEnvFloatOrDefault("RECEIPT_REQUIRED_THRESHOLD", 25.0),

		ConfigFeatureFlagServiceURL: getEnvOrDefault("CONFIG_FEATURE_FLAG_SERVICE_URL", "http://configuration-feature-flag-svc:8086"),
		Environment:                 getEnvOrDefault("ENVIRONMENT", "production"),

		ReimbursementTermsDays:     getEnvFloatOrDefault("REIMBURSEMENT_TERMS_DAYS", 14),
		PolicyControlledCategories: splitCSV(getEnvOrDefault("POLICY_CONTROLLED_CATEGORIES", "*")),

		GeneralLedgerServiceURL:  getEnvOrDefault("GENERAL_LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		AccountingPrincipalID:    os.Getenv("ACCOUNTING_PRINCIPAL_ID"),
		PostingExpenseKey:        getEnvOrDefault("POSTING_EXPENSE_MAPPING_KEY", "AP07_EXPENSE_CLAIM_EXPENSE"),
		PostingPayableKey:        getEnvOrDefault("POSTING_PAYABLE_MAPPING_KEY", "AP07_EXPENSE_CLAIM_PAYABLE"),
		PostingTaxRecoverableKey: getEnvOrDefault("POSTING_TAX_RECOVERABLE_MAPPING_KEY", "AP07_EXPENSE_CLAIM_TAX_RECOVERABLE"),
		FiscalPeriodLayout:       getEnvOrDefault("FISCAL_PERIOD_LAYOUT", "2006-01"),
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

func getEnvFloatOrDefault(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
