package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for authorization-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is used only when creating a jurisdiction-scoped
	// SoD rule — see internal/jurisdiction.HTTPValidator.
	JurisdictionRulesURL string
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

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8089: 8080-8088 are already taken by every other Phase 1 service
		// built so far — see services/README.md.
		Port: envInt("PORT", 8089),
		// DB_NAME defaults to "authorization_svc", not "authorization" —
		// AUTHORIZATION is a reserved SQL keyword (CREATE SCHEMA ...
		// AUTHORIZATION owner), so a bare CREATE DATABASE authorization
		// fails with a syntax error. Avoids needing to quote the
		// identifier everywhere, forever.
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "authorization_svc"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "authorization-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.authorization.events"),
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
