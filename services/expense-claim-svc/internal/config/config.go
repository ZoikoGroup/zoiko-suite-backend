package config

import (
	"fmt"
	"os"
	"strconv"
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
	ReceiptRequiredThreshold   float64
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
		ReceiptRequiredThreshold:   getEnvFloatOrDefault("RECEIPT_REQUIRED_THRESHOLD", 25.0),
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
