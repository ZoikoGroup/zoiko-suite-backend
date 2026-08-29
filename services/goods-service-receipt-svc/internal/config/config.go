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
	GRNIDebitAccountCode    string
	GRNICreditAccountCode   string
	OverReceiptTolerancePct float64
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
		GRNIDebitAccountCode:    getEnvOrDefault("GRNI_DEBIT_ACCOUNT_CODE", "5100-GRNI-CLEARING"),
		GRNICreditAccountCode:   getEnvOrDefault("GRNI_CREDIT_ACCOUNT_CODE", "2100-GRNI-ACCRUAL"),
		OverReceiptTolerancePct: getEnvFloatOrDefault("OVER_RECEIPT_TOLERANCE_PCT", 0.0),
	}, nil
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

func (c *Config) DSN() string {
	return c.DatabaseURL
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
