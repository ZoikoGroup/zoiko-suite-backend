package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for workflow-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// AuthorizationServiceURL is called synchronously on every approval
	// action submission — see internal/authz.HTTPClient.
	AuthorizationServiceURL string
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
		// 8090: 8080-8089 are already taken by every other Phase 1 service
		// built so far — see services/README.md.
		Port: envInt("PORT", 8090),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "workflow"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "workflow-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.workflow.events"),
		},
		AuthorizationServiceURL: env("AUTHORIZATION_SERVICE_URL", "http://authorization-svc:8089"),
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
