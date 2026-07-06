// Package config holds runtime configuration for audit-event-store-svc.
// All values are sourced from environment variables with safe defaults.
// No jurisdiction, currency, or tax-rule value is ever hardcoded here —
// see .agents/rules/doctrine.md.
package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration for audit-event-store-svc.
type Config struct {
	// Port is the HTTP port the service listens on.
	Port int

	// DB is the PostgreSQL connection configuration.
	DB DBConfig
}

// DBConfig carries the PostgreSQL connection parameters.
type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

// DSN returns a libpq-style connection string suitable for pgxpool.ParseConfig.
func (d DBConfig) DSN() string {
	return "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + d.Password +
		" sslmode=" + d.SSLMode
}

// Load reads config from the environment. It never returns an error in the
// current implementation (all fields have safe defaults), but the error return
// is preserved for future validation logic.
func Load() (*Config, error) {
	return &Config{
		Port: envInt("PORT", 8080),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "audit_event_store"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
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
