// Package config loads schema-registry-svc configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string
}

func (d DBConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode)
}

type Config struct {
	Env  string
	Port int
	DB   DBConfig
	// AuthorizationServiceURL is authorization-svc's base URL (no trailing
	// slash). Schema registration is gated through it, fail-closed.
	AuthorizationServiceURL string

	// AuthZPlatformScopeID is the synthetic legal entity used to authorize
	// registrations that carry no legal entity of their own.
	//
	// An event contract belongs to the platform, not to a legal entity —
	// `zoiko.invoice.approved` means the same thing in every entity. But
	// authorization-svc rejects an empty legal_entity_id outright, and this
	// service passed the header through verbatim, so a caller that did not
	// send one got a 400 from authorization-svc, which this client reports as
	// "authorization service unavailable" — a 503 blaming infrastructure for a
	// scope the request was never going to have. Same synthetic scope, same
	// reasoning, as jurisdiction-rules-svc.
	AuthZPlatformScopeID string
}

func Load() (*Config, error) {
	port, err := intEnv("PORT", 8093)
	if err != nil {
		return nil, err
	}
	dbPort, err := intEnv("DB_PORT", 5432)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Env:  strEnv("ENV", "local"),
		Port: port,
		DB: DBConfig{
			Host:     strEnv("DB_HOST", "localhost"),
			Port:     dbPort,
			Name:     strEnv("DB_NAME", "schema_registry"),
			User:     strEnv("DB_USER", "postgres"),
			Password: strEnv("DB_PASSWORD", "postgres"),
			SSLMode:  strEnv("DB_SSLMODE", "disable"),
		},
		AuthorizationServiceURL: strEnv("AUTHORIZATION_SERVICE_URL", "http://authorization-svc:8089"),
		AuthZPlatformScopeID:    strEnv("AUTHZ_PLATFORM_SCOPE_ID", "00000000-0000-0000-0000-00000000f001"),
	}

	// Load returned a nil error unconditionally, so every default above was
	// also a production default — including DB_PASSWORD "postgres" and
	// DB_SSLMODE "disable". A governed registry that starts on a hardcoded
	// password over an unencrypted connection is worse than one that refuses
	// to start.
	if cfg.Env == "production" {
		var problems []string
		if cfg.DB.Password == "" || cfg.DB.Password == "postgres" {
			problems = append(problems, "DB_PASSWORD must be set to a real secret")
		}
		if cfg.DB.SSLMode == "disable" {
			problems = append(problems, "DB_SSLMODE must not be 'disable'")
		}
		if strings.Contains(cfg.AuthorizationServiceURL, "localhost") {
			problems = append(problems, "AUTHORIZATION_SERVICE_URL must be a real authorization-svc address")
		}
		if len(problems) > 0 {
			return nil, fmt.Errorf("invalid production config: %s", strings.Join(problems, "; "))
		}
	}

	return cfg, nil
}

func strEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return n, nil
}
