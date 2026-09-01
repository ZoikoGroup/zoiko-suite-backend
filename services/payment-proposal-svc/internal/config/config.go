package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                       string
	DatabaseURL                string
	KafkaBrokers               string
	KafkaEventsTopic           string
	AuthzServiceURL            string
	AccountsPayableServiceURL  string
	PayableOpenItemServiceURL  string
	SupplierProfileServiceURL  string
	TaxDeterminationServiceURL string
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8159"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                       port,
		DatabaseURL:                dbURL,
		KafkaBrokers:               getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:           getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-proposal.events"),
		AuthzServiceURL:            getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		AccountsPayableServiceURL:  getEnvOrDefault("ACCOUNTS_PAYABLE_SERVICE_URL", "http://accounts-payable-svc:8099"),
		PayableOpenItemServiceURL:  getEnvOrDefault("PAYABLE_OPEN_ITEM_SERVICE_URL", "http://payable-open-item-svc:8164"),
		SupplierProfileServiceURL:  getEnvOrDefault("SUPPLIER_PROFILE_SERVICE_URL", "http://supplier-financial-profile-svc:8156"),
		TaxDeterminationServiceURL: getEnvOrDefault("TAX_DETERMINATION_SERVICE_URL", "http://tax-determination-svc:8126"),
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
