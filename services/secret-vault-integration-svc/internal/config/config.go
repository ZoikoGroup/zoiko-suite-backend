// Package config provides typed, env-driven configuration for secret-vault-integration-svc.
package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration.
type Config struct {
	Port int

	// Vault backend: "hashicorp", "aws-secretsmanager", "gcp-secretmanager", "azure-keyvault", "local"
	VaultBackend string

	// HashiCorp Vault
	VaultAddr      string
	VaultToken     string
	VaultNamespace string
	VaultMountPath string // e.g., "secret/data/zoiko"

	// AWS Secrets Manager
	AWSRegion          string
	AWSSecretsPrefix   string
	AWSKMSKeyID        string
	AWSAccessKeyID     string
	AWSSecretAccessKey string

	// GCP Secret Manager
	GCPProjectID string
	GCPPrefix    string

	// Azure Key Vault
	AzureVaultURL string
	AzureTenantID string
	AzureClientID string
	AzureClientSecret string

	// Local file-based (dev only)
	LocalStorePath string

	DB    DBConfig
	Redis RedisConfig
	Kafka KafkaConfig

	OTELExporterEndpoint string
	SIEMServiceURL       string
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

type RedisConfig struct {
	Host string
	Port int
	TTL  int
}

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{
		Port: envInt("PORT", 8087),

		VaultBackend: env("VAULT_BACKEND", "local"),

		VaultAddr:      env("VAULT_ADDR", "http://vault:8200"),
		VaultToken:     env("VAULT_TOKEN", ""),
		VaultNamespace: env("VAULT_NAMESPACE", ""),
		VaultMountPath: env("VAULT_MOUNT_PATH", "secret/data/zoiko"),

		AWSRegion:          env("AWS_REGION", "us-east-1"),
		AWSSecretsPrefix:   env("AWS_SECRETS_PREFIX", "zoiko/"),
		AWSKMSKeyID:        env("AWS_KMS_KEY_ID", ""),
		AWSAccessKeyID:     env("AWS_ACCESS_KEY_ID", ""),
		AWSSecretAccessKey: env("AWS_SECRET_ACCESS_KEY", ""),

		GCPProjectID: env("GCP_PROJECT_ID", ""),
		GCPPrefix:    env("GCP_PREFIX", "zoiko-"),

		AzureVaultURL:      env("AZURE_VAULT_URL", ""),
		AzureTenantID:      env("AZURE_TENANT_ID", ""),
		AzureClientID:      env("AZURE_CLIENT_ID", ""),
		AzureClientSecret:  env("AZURE_CLIENT_SECRET", ""),

		LocalStorePath: env("LOCAL_STORE_PATH", "./vault-data"),

		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "secret_vault"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Redis: RedisConfig{
			Host: env("REDIS_HOST", "localhost"),
			Port: envInt("REDIS_PORT", 6379),
			TTL:  envInt("REDIS_TTL_SECONDS", 300),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "secret-vault-integration-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.secrets.events"),
		},
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
		SIEMServiceURL:       env("SIEM_SERVICE_URL", ""),
	}

	// Validate required config for non-local backends
	if cfg.VaultBackend != "local" {
		switch cfg.VaultBackend {
		case "hashicorp":
			if cfg.VaultToken == "" {
				return nil, errors.New("VAULT_TOKEN required for hashicorp backend")
			}
		case "aws-secretsmanager":
			if cfg.AWSAccessKeyID == "" || cfg.AWSSecretAccessKey == "" {
				return nil, errors.New("AWS credentials required for aws-secretsmanager backend")
			}
		case "gcp-secretmanager":
			if cfg.GCPProjectID == "" {
				return nil, errors.New("GCP_PROJECT_ID required for gcp-secretmanager backend")
			}
		case "azure-keyvault":
			if cfg.AzureVaultURL == "" || cfg.AzureClientID == "" || cfg.AzureClientSecret == "" {
				return nil, errors.New("Azure credentials required for azure-keyvault backend")
			}
		default:
			return nil, errors.New("unknown VAULT_BACKEND: " + cfg.VaultBackend)
		}
	}

	return cfg, nil
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