package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Env          string
	Port         string
	DatabaseURL  string
	KafkaBrokers string
	KafkaTopic   string
	AuthzURL     string
	LogLevel     string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthzURL (plain HTTP) keeps being used unless
	// explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	// EventResidencyRegion is the residencyregion every event this service
	// emits carries (ZS-EVENT-001 §4: required, "authoritative regional
	// data-bearing context"). Deliberately NO default: a wrong region silently
	// stamped on every evidence event is worse than refusing to start.
	// EVENT_RESIDENCY_REGION, a lower-case code such as "uk" or "eu-west".
	EventResidencyRegion string

	// IntegrityScoreThreshold is the acceptance bar for a validation run, as
	// a percentage: a job scoring below it is FAILED rather than COMPLETED.
	// The bar actually applied is stored on every job, so the acceptance
	// decision stays auditable after the configured default changes.
	// INTEGRITY_SCORE_THRESHOLD; defaults to 95.
	IntegrityScoreThreshold float64
}

func Load() (*Config, error) {
	region := os.Getenv("EVENT_RESIDENCY_REGION")
	if region == "" {
		return nil, errors.New("EVENT_RESIDENCY_REGION is required (e.g. uk, eu-west): every emitted event must carry its residency region")
	}

	// Refused rather than clamped: silently bending a "101" or "abc" bar
	// into something workable would accept migrations against a threshold
	// nobody configured while the job record shows the number that was
	// asked for.
	thresholdRaw := getEnv("INTEGRITY_SCORE_THRESHOLD", "95")
	threshold, err := strconv.ParseFloat(thresholdRaw, 64)
	if err != nil || threshold < 0 || threshold > 100 {
		return nil, fmt.Errorf("INTEGRITY_SCORE_THRESHOLD must be a percentage between 0 and 100, got %q", thresholdRaw)
	}

	return &Config{
		EventResidencyRegion:    region,
		IntegrityScoreThreshold: threshold,
		Env:                     getEnv("ENV", "local"),
		Port:                    getEnv("PORT", "8139"),
		DatabaseURL:             getEnv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/migration_integrity?sslmode=disable"),
		KafkaBrokers:            getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaTopic:              getEnv("KAFKA_TOPIC", "zoiko.migration-integrity.events"),
		AuthzURL:                getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089"),
		LogLevel:                getEnv("LOG_LEVEL", "info"),

		AuthzMTLSEnabled:         getEnv("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             getEnv("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: getEnv("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
