package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for obligations-svc.
//
// No AuthZServiceURL field: admin writes do not call Authorization Service
// yet — it doesn't exist. Deliberate, documented deferral matching
// policy-svc's and governance-decision-log-svc's precedent of shipping
// without it.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is the base URL of jurisdiction-rules-svc. Called
	// synchronously on obligation creation to validate jurisdiction_id
	// (critical constraint: every obligation must be jurisdiction-bound) —
	// see internal/jurisdiction.HTTPValidator.
	JurisdictionRulesURL string
}

// DBConfig holds PostgreSQL connection parameters.
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

// KafkaConfig mirrors identity-context-svc's, tenant-entity-registry-svc's,
// and policy-svc's shape exactly. GroupID is unused today (this service
// only produces), kept for shape consistency with the rest of the platform.
type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8088: 8080-8087 are already taken by every other Phase 1 service
		// built so far — secret-vault-integration-svc claimed 8087 while this
		// service was in flight on a separate branch — see services/README.md.
		Port: envInt("PORT", 8088),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "obligations"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "obligations-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.obligations.events"),
		},
		JurisdictionRulesURL: env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8082"),
	}, nil
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
