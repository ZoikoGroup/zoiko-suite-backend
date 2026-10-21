package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port             string
	DatabaseURL      string
	KafkaBrokers     string
	KafkaEventsTopic string
	AuthzServiceURL  string
	// TreasuryServiceURL is the base URL of treasury-svc, used by
	// PrepareAttempt to verify PayerAccountRef for real (BNK-01's
	// account_status + is_ownership_verified) instead of trusting the
	// caller-supplied PayerAccountVerified flag alone.
	TreasuryServiceURL string

	// PayrollRunServiceURL is payroll-run-svc, which verifies WFP-07 payroll payment instructions.
	PayrollRunServiceURL string
	// AuthorizationServiceURL is the base URL of payment-authorization-svc
	// (AP-10), used by PrepareAttempt to independently re-verify an
	// AuthorizationSourcePaymentAuthorization fingerprint rather than
	// trusting the caller-supplied value (Wave 11a).
	AuthorizationServiceURL string
	// EventResidencyRegion is the residency region carried by emitted
	// events (ZS-EVENT-001 §4). Empty default — deployments must set it.
	EventResidencyRegion string

	// Provider adapters (ZS-SVC-M-001 INT-07). Off by default: they need a connector service to resolve
	// approved connectors and a vault to release credentials, and an explicit egress policy.
	Providers ProviderConfig
}

// ProviderConfig configures the live bank/PSP adapter.
type ProviderConfig struct {
	Enabled         bool
	ConnectivityURL string // connectivity-api-bridge-svc
	VaultURL        string // secret-vault-integration-svc
	WorkloadID      string // this service's identity at the vault; the secret policy must allow it
	// AllowHTTP is for local development only.
	AllowHTTP    bool
	AllowedPorts []int
	// PrivateCIDRs are private ranges a provider may be reached on (dedicated links, VPN, on-prem gateways).
	PrivateCIDRs    []string
	StaleAfter      time.Duration
	SweepInterval   time.Duration
	BreakerWindow   time.Duration
	BreakerFailures int
	MaxResponse     int64
}

func Load() (*Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8162"
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return &Config{
		Port:                    port,
		DatabaseURL:             dbURL,
		KafkaBrokers:            getEnvOrDefault("KAFKA_BROKERS", "kafka:9092"),
		KafkaEventsTopic:        getEnvOrDefault("KAFKA_EVENTS_TOPIC", "zoiko.payment-initiation.events"),
		AuthzServiceURL:         getEnvOrDefault("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		TreasuryServiceURL:      getEnvOrDefault("TREASURY_SERVICE_URL", "http://treasury-svc:8103"),
		PayrollRunServiceURL:    getEnvOrDefault("PAYROLL_RUN_SERVICE_URL", "http://payroll-run-svc:8110"),
		AuthorizationServiceURL: getEnvOrDefault("AUTHORIZATION_SERVICE_URL", "http://payment-authorization-svc:8160"),
		EventResidencyRegion:    os.Getenv("EVENT_RESIDENCY_REGION"),
		Providers:               loadProviders(),
	}, nil
}

func loadProviders() ProviderConfig {
	c := ProviderConfig{
		Enabled:         os.Getenv("PROVIDER_ADAPTERS_ENABLED") == "true",
		ConnectivityURL: getEnvOrDefault("CONNECTIVITY_SERVICE_URL", "http://connectivity-api-bridge-svc:8118"),
		VaultURL:        getEnvOrDefault("VAULT_SERVICE_URL", "http://secret-vault-integration-svc:8087"),
		WorkloadID:      getEnvOrDefault("VAULT_WORKLOAD_ID", "payment-initiation-adapter-svc"),
		AllowHTTP:       os.Getenv("PROVIDER_ALLOW_HTTP") == "true",
		StaleAfter:      envDuration("PROVIDER_STALE_AFTER", 2*time.Minute),
		SweepInterval:   envDuration("PROVIDER_SWEEP_INTERVAL", 30*time.Second),
		BreakerWindow:   envDuration("PROVIDER_BREAKER_WINDOW", time.Minute),
		BreakerFailures: envInt("PROVIDER_BREAKER_FAILURES", 5),
		MaxResponse:     int64(envInt("PROVIDER_MAX_RESPONSE_BYTES", 256<<10)),
	}
	for _, p := range strings.Split(os.Getenv("PROVIDER_ALLOWED_PORTS"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			c.AllowedPorts = append(c.AllowedPorts, n)
		}
	}
	for _, p := range strings.Split(os.Getenv("PROVIDER_PRIVATE_CIDRS"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.PrivateCIDRs = append(c.PrivateCIDRs, p)
		}
	}
	return c
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
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
