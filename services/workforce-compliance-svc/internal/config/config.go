package config

import (
	"fmt"
	"os"
)

type Config struct {
	Port                 string
	DBHost               string
	DBPort               string
	DBName               string
	DBUser               string
	DBPassword           string
	DBSslMode            string
	KafkaBrokers         string
	KafkaEventsTopic     string
	EmployeeMasterURL    string
	JurisdictionRulesURL  string
	AuthzServiceURL      string
}

func Load() (*Config, error) {
	cfg := &Config{
		Port:                 getEnv("PORT", "8118"),
		DBHost:               getEnv("DB_HOST", "localhost"),
		DBPort:               getEnv("DB_PORT", "5432"),
		DBName:               getEnv("DB_NAME", "workforce_compliance"),
		DBUser:               getEnv("DB_USER", "postgres"),
		DBPassword:           getEnv("DB_PASSWORD", "postgres"),
		DBSslMode:            getEnv("DB_SSLMODE", "disable"),
		KafkaBrokers:         getEnv("KAFKA_BROKERS", "localhost:9092"),
		KafkaEventsTopic:     getEnv("KAFKA_EVENTS_TOPIC", "zoiko.compliance.events"),
		EmployeeMasterURL:    getEnv("EMPLOYEE_MASTER_URL", "http://localhost:8108"),
		JurisdictionRulesURL: getEnv("JURISDICTION_RULES_URL", "http://localhost:8081"),
		AuthzServiceURL:      getEnv("AUTHZ_SERVICE_URL", "http://localhost:8089"),
	}

	return cfg, nil
}

func (c *Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSslMode,
	)
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
