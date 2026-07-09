// Package config loads schema-registry-svc configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
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
	Port int
	DB   DBConfig
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

	return &Config{
		Port: port,
		DB: DBConfig{
			Host:     strEnv("DB_HOST", "localhost"),
			Port:     dbPort,
			Name:     strEnv("DB_NAME", "schema_registry"),
			User:     strEnv("DB_USER", "postgres"),
			Password: strEnv("DB_PASSWORD", "postgres"),
			SSLMode:  strEnv("DB_SSLMODE", "disable"),
		},
	}, nil
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
