// Package config loads evidence-manifest-svc configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type DBConfig struct {
	Host, Name, User, Password, SSLMode string
	Port                                int
}

func (c DBConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.User, c.Password, c.Host, c.Port, c.Name, c.SSLMode)
}

type KafkaConfig struct {
	Brokers []string
	Topic   string
}

type Config struct {
	Port  int
	DB    DBConfig
	Kafka KafkaConfig

	GovernanceDecisionLogURL string
	AuthorizationServiceURL  string
	WorkflowServiceURL       string
}

func Load() (*Config, error) {
	port, err := intEnv("PORT", 8095)
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
			Name:     strEnv("DB_NAME", "evidence_manifest"),
			User:     strEnv("DB_USER", "postgres"),
			Password: strEnv("DB_PASSWORD", ""),
			SSLMode:  strEnv("DB_SSLMODE", "disable"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(strEnv("KAFKA_BROKERS", "localhost:9092"), ","),
			Topic:   strEnv("KAFKA_EVENTS_TOPIC", "zoiko.evidence.events"),
		},
		GovernanceDecisionLogURL: strEnv("GOVERNANCE_DECISION_LOG_SERVICE_URL", "http://governance-svc:8083"),
		AuthorizationServiceURL:  strEnv("AUTHORIZATION_SERVICE_URL", "http://authorization-svc:8089"),
		WorkflowServiceURL:       strEnv("WORKFLOW_SERVICE_URL", "http://workflow-svc:8090"),
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
