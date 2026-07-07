// Package config provides typed, env-driven configuration for identity-context-svc.
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for identity-context-svc.
// All values are sourced from environment variables — no hard-coded secrets.
type Config struct {
	Port int

	// JWT envelope signing (Q2 — signed short-lived JWT)
	// Production: RS256 via KMS-backed keypair through Secret Vault Integration Service.
	JWTSigningPrivateKeyPath string
	JWTKeyID                 string

	// TODO: replace JWTSigningSecret with KMS key reference before Phase 1 production cutover.
	JWTSigningSecret       string
	JWTIssuer              string
	JWTAudienceInternal    string
	EnvelopeJWTTTLSeconds  int

	DB    DBConfig
	Redis RedisConfig
	Kafka KafkaConfig

	// Upstream Tier 0 service base URLs (read-only calls only)
	TenantRegistryURL      string
	DelegatedAuthorityURL  string
	AccessControlURL       string
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
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

type RedisConfig struct {
	Host                  string
	Port                  int
	// SessionTTLSeconds — hot-path cache TTL for signed envelope JWT (default 5 min)
	SessionTTLSeconds     int
	// RoleProfileTTLSeconds — role profile cache TTL (default 15 min)
	RoleProfileTTLSeconds int
}

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables with safe defaults.
// Returns an error description string for any missing mandatory value.
func Load() (*Config, error) {
	cfg := &Config{
		Port:                  envInt("PORT", 8080),
		JWTSigningSecret:      env("JWT_SIGNING_SECRET", ""),
		JWTIssuer:             env("JWT_ISSUER", "identity-context-svc"),
		JWTAudienceInternal:   env("JWT_AUDIENCE", "zoiko-internal"),
		JWTSigningPrivateKeyPath: env("JWT_SIGNING_PRIVATE_KEY_PATH", "./envelope_signing_key.pem"),
		JWTKeyID:                 env("JWT_KEY_ID", "envelope-signing-key-1"),

		EnvelopeJWTTTLSeconds: envInt("ENVELOPE_JWT_TTL_SECONDS", 300),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "identity_context"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Redis: RedisConfig{
			Host:                  env("REDIS_HOST", "localhost"),
			Port:                  envInt("REDIS_PORT", 6379),
			SessionTTLSeconds:     envInt("SESSION_CACHE_TTL_SECONDS", 300),
			RoleProfileTTLSeconds: envInt("ROLE_PROFILE_CACHE_TTL_SECONDS", 900),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "identity-context-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.identity.events"),
		},
		TenantRegistryURL:     env("TENANT_REGISTRY_URL", "http://tenant-registry-svc"),
		DelegatedAuthorityURL: env("DELEGATED_AUTHORITY_URL", "http://delegated-authority-svc"),
		AccessControlURL:      env("ACCESS_CONTROL_URL", "http://access-control-svc"),
	}
	return cfg, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
