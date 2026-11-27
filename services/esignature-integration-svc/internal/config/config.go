package config

import "os"

type Config struct {
	Port             string
	DatabaseURL      string
	KafkaBrokers     string
	KafkaEventsTopic string
	AuthzServiceURL  string

	// AuthzMTLSEnabled opts this service into calling authorization-svc over
	// mutual TLS instead of plain HTTP. OFF by default — see internal/mtls.
	AuthzMTLSEnabled bool
	AuthzMTLSURL     string

	MTLSManagementServiceURL string

	// ESignProviders is the comma-separated list of provider adapters to
	// enable (ESIGN_PROVIDERS). Default empty = fail closed: no envelope can
	// be sent and no provider-asserted status recorded. No adapter is
	// implemented in this build, so any non-empty value fails startup.
	ESignProviders string
}

func Load() *Config {
	return &Config{
		Port:                     getEnv("PORT", "8148"),
		DatabaseURL:              getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/zoiko_esignature_integration?sslmode=disable"),
		KafkaBrokers:             getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic:         getEnv("KAFKA_EVENTS_TOPIC", "zoiko.esignature.integration.events"),
		AuthzServiceURL:          getEnv("AUTHZ_SERVICE_URL", "http://localhost:8081"),
		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		ESignProviders:           os.Getenv("ESIGN_PROVIDERS"),
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
