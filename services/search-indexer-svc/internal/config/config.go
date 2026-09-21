// Package config loads search-indexer-svc's runtime configuration from the
// environment. No config file — same posture as every other service here.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds everything the process needs to start.
type Config struct {
	Env  string
	Port int

	DB         DBConfig
	Kafka      KafkaConfig
	OpenSearch OpenSearchConfig

	// AuthZServiceURL is authorization-svc. Both the control-plane writes
	// (contracts, generations, restrictions) and the R1/R2 retrieval
	// re-authorization go there. This service authorizes nothing itself
	// (03-microservices.md §17.1).
	AuthZServiceURL string

	// AuthZPlatformScopeID is the legal_entity_id presented when the act has
	// no entity dimension — publishing a contract, activating a generation.
	// authorization-svc rejects an empty legal_entity_id outright, and the
	// PLATFORM sentinel resolves to this id on its side (tracker row 67).
	AuthZPlatformScopeID string

	// CursorSigningKey signs pagination cursors. NP-57: "cursor is modified to
	// increase scope/tenant → signed opaque cursor fails integrity
	// validation." Without a key there is no integrity check, so this has no
	// default outside local development and the service refuses to start.
	CursorSigningKey []byte

	// SourceHydrationTimeout bounds an R2 hydration call. A hit whose source
	// cannot be fetched inside it is SUPPRESSED, never served from the index
	// (NP-20).
	SourceHydrationTimeout time.Duration

	// RestrictionVerifyInterval is how often the propagation verifier sweeps
	// APPLIED tombstones and proves invisibility. §8.2 requires verification
	// to be independent of the write that applied it.
	RestrictionVerifyInterval time.Duration

	// CheckpointInterval is how often freshness and population counts are
	// recomputed into index_checkpoints.
	CheckpointInterval time.Duration

	// MaxResultWindow is the hard ceiling on a single page (ESR-015).
	MaxResultWindow int

	// MaxComplexityScore bounds a compiled plan's cost (ESR-004).
	MaxComplexityScore int

	// FacetMinCount is the minimum-cell rule for facet buckets (§7.3, NP-08).
	// OD-08 leaves the threshold to PDC/PRV per jurisdiction; this is the
	// platform default until they ratify one.
	FacetMinCount int

	OTELExporterEndpoint string
}

type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

func (d DBConfig) DSN() string {
	return fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
		d.Host, d.Port, d.Name, d.User, d.Password, d.SSLMode)
}

type KafkaConfig struct {
	Brokers []string
	GroupID string
	// Topics is resolved at runtime from the registered search sources, not
	// from configuration. A source registers its own event_topic (§5.1), so
	// wiring topics here would mean a registration that cannot take effect
	// without a redeploy. This field is only the bootstrap set used before any
	// source exists.
	BootstrapTopics []string
}

type OpenSearchConfig struct {
	Addresses []string
	Username  string
	Password  string
}

// Load reads the environment and validates it. Returns an error rather than
// exiting so main can log it through the service's own logger.
func Load() (*Config, error) {
	c := &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8096),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "search_indexer"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", "postgres"),
			SSLMode:  env("DB_SSLMODE", "disable"),
		},
		Kafka: KafkaConfig{
			Brokers:         splitCSV(env("KAFKA_BROKERS", "localhost:9092")),
			GroupID:         env("KAFKA_GROUP_ID", "search-indexer-svc"),
			BootstrapTopics: splitCSV(env("KAFKA_BOOTSTRAP_TOPICS", "")),
		},
		OpenSearch: OpenSearchConfig{
			Addresses: splitCSV(env("OPENSEARCH_ADDRESSES", "http://localhost:9200")),
			Username:  os.Getenv("OPENSEARCH_USERNAME"),
			Password:  os.Getenv("OPENSEARCH_PASSWORD"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", ""),
		AuthZPlatformScopeID: env("AUTHZ_PLATFORM_SCOPE_ID", ""),
		MaxResultWindow:      envInt("MAX_RESULT_WINDOW", 100),
		MaxComplexityScore:   envInt("MAX_COMPLEXITY_SCORE", 100),
		FacetMinCount:        envInt("FACET_MIN_COUNT", 2),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}

	var err error
	if c.SourceHydrationTimeout, err = envDuration("SOURCE_HYDRATION_TIMEOUT", 3*time.Second); err != nil {
		return nil, err
	}
	if c.RestrictionVerifyInterval, err = envDuration("RESTRICTION_VERIFY_INTERVAL", 30*time.Second); err != nil {
		return nil, err
	}
	if c.CheckpointInterval, err = envDuration("CHECKPOINT_INTERVAL", 60*time.Second); err != nil {
		return nil, err
	}

	if c.CursorSigningKey, err = loadCursorKey(c.Env); err != nil {
		return nil, err
	}

	if len(c.OpenSearch.Addresses) == 0 {
		return nil, errors.New("OPENSEARCH_ADDRESSES must not be empty")
	}
	if len(c.Kafka.Brokers) == 0 {
		return nil, errors.New("KAFKA_BROKERS must not be empty")
	}
	if c.MaxResultWindow <= 0 {
		return nil, fmt.Errorf("MAX_RESULT_WINDOW must be positive, got %d", c.MaxResultWindow)
	}
	// A minimum cell of 1 is no suppression at all, and 0 or negative would be
	// read by the engine as "return every bucket". NP-08 is specifically about
	// a facet count of one revealing a single privileged record, so 2 is the
	// lowest value that means anything.
	if c.FacetMinCount < 2 {
		return nil, fmt.Errorf("FACET_MIN_COUNT must be at least 2 to suppress single-record facets, got %d", c.FacetMinCount)
	}
	return c, nil
}

// loadCursorKey reads the HMAC key that makes pagination cursors tamper-evident.
//
// Outside local development this has NO default. A fixed fallback key would be
// public the moment this repository is read, and a cursor signed with a public
// key is not signed — NP-57's "cursor is modified to increase scope/tenant"
// becomes a forgery anyone can perform. Local development gets a fixed
// development key so the service starts on a laptop without ceremony, and the
// value is obviously a development one to anyone who sees it in a log.
func loadCursorKey(environment string) ([]byte, error) {
	raw := os.Getenv("CURSOR_SIGNING_KEY_HEX")
	if raw == "" {
		if environment == "local" {
			return []byte("local-development-cursor-key-not-for-production"), nil
		}
		return nil, errors.New("CURSOR_SIGNING_KEY_HEX is required outside local development: " +
			"an unsigned cursor can be edited to widen scope or change tenant (NP-57)")
	}
	key, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("CURSOR_SIGNING_KEY_HEX must be hex-encoded: %w", err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("CURSOR_SIGNING_KEY_HEX must be at least 32 bytes, got %d", len(key))
	}
	return key, nil
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", key, v, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", key, d)
	}
	return d, nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
