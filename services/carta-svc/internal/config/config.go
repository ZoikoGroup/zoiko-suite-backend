// Package config reads the service's environment.
package config

import (
	"errors"
	"os"
)

// Config is everything cmd/server needs to start.
type Config struct {
	Port            string
	DatabaseURL     string
	AuthzServiceURL string
	// KafkaBrokers (comma-separated) is where the outbox relay delivers
	// events. Optional: when unset the relay does not run and events stay in
	// outbox_events, unpublished but not lost, until it is configured.
	KafkaBrokers string
	// KafkaTopic defaults to zoiko.carta.events.
	KafkaTopic string
}

// Load fails rather than defaulting a dependency the service cannot run
// without. DATABASE_URL: the assessments are the authoritative decision
// records and live only in Postgres. AUTHZ_SERVICE_URL: every guarded route
// would 503 without it (see cmd/server).
func Load() (*Config, error) {
	cfg := &Config{
		Port:            os.Getenv("PORT"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		AuthzServiceURL: os.Getenv("AUTHZ_SERVICE_URL"),
		KafkaBrokers:    os.Getenv("KAFKA_BROKERS"),
		KafkaTopic:      os.Getenv("KAFKA_TOPIC"),
	}
	if cfg.KafkaTopic == "" {
		cfg.KafkaTopic = "zoiko.carta.events"
	}
	if cfg.Port == "" {
		cfg.Port = "8142"
	}
	if cfg.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required: assessments are persisted only in Postgres (database carta)")
	}
	if cfg.AuthzServiceURL == "" {
		return nil, errors.New("AUTHZ_SERVICE_URL is required: every authorized route would return 503 without it")
	}
	return cfg, nil
}
