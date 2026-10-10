package config

import (
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Port                    string
	DatabaseURL             string
	KafkaBrokers            string
	KafkaEventsTopic        string
	AuthzServiceURL         string
	PurchaseOrderServiceURL string
	GeneralLedgerServiceURL string
	// OverReceiptTolerancePct is a PERCENTAGE of the ordered quantity (5 = 5%) a
	// receipt may exceed the open quantity by without an approved exception — the
	// same unit as purchase-order-svc's PO_OVER_TOLERANCE_PERCENT.
	OverReceiptTolerancePct float64

	// GRNI posting (ACC-04). AccountingPrincipalID is the service identity sent as
	// X-Principal-Id and must hold the ledger's GL_POSTING_EXECUTE action; when it
	// is empty the dispatcher is not started and posting requests simply stay
	// PENDING (nothing is lost, nothing is posted under an invented identity).
	AccountingPrincipalID string
	// POProgressPrincipalID is the service identity sent to purchase-order-svc
	// when delivering received-quantity deltas; it must hold PO_PROGRESS_RECORD.
	// When empty the progress worker is not started and pushes stay PENDING
	// (visible on the receipt, counted against the line's open quantity).
	POProgressPrincipalID string
	// ACC-02 mapping keys the accrual posts against; the ledger resolves them to
	// accounts, so this service never names a GL account.
	GRNIDebitMappingKey  string
	GRNICreditMappingKey string
	PostingPolicyVersion string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8157"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                    port,
		DatabaseURL:             dbURL,
		KafkaBrokers:            getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:        getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.goods-service-receipt.events"),
		AuthzServiceURL:         getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		PurchaseOrderServiceURL: getEnvOrDefault("PURCHASE_ORDER_SERVICE_URL", "http://purchase-order-svc:8129"),
		GeneralLedgerServiceURL: getEnvOrDefault("GENERAL_LEDGER_SERVICE_URL", "http://general-ledger-svc:8098"),
		OverReceiptTolerancePct: getEnvFloatOrDefault("OVER_RECEIPT_TOLERANCE_PCT", 0.0),
		AccountingPrincipalID:   os.Getenv("ACCOUNTING_PRINCIPAL_ID"),
		POProgressPrincipalID:   os.Getenv("PO_PROGRESS_PRINCIPAL_ID"),
		GRNIDebitMappingKey:     getEnvOrDefault("GRNI_DEBIT_MAPPING_KEY", "AP_GRNI_EXPENSE"),
		GRNICreditMappingKey:    getEnvOrDefault("GRNI_CREDIT_MAPPING_KEY", "AP_GRNI_ACCRUAL"),
		PostingPolicyVersion:    getEnvOrDefault("GRNI_POSTING_POLICY_VERSION", "grni-v1"),
	}, nil
}

func getEnvFloatOrDefault(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
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
