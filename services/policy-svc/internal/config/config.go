package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration for policy-svc.
//
// No AuthZServiceURL field: admin writes (create/activate) do not call
// Authorization Service yet — it doesn't exist. This is a deliberate,
// documented deferral (see CONTEXT.md §11/§13), not an oversight, and
// matches governance-decision-log-svc's precedent of shipping without it.
type Config struct {
	Env  string
	Port int

	DB DBConfig
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

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	return &Config{
		Env: env("ENV", "local"),
		// 8085, not 8084: 8080-8083 are taken (identity/tenant-entity/
		// jurisdiction-rules/governance-decision-log). 8084 is reserved
		// defensively for configuration-feature-flag-svc in case it lands
		// first — see PROGRESS.md.
		Port: envInt("PORT", 8085),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "policy"),
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
