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
