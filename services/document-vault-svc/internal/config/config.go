// Package config loads document-vault-svc configuration from the environment.
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

func (c DBConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.User, c.Password, c.Host, c.Port, c.Name, c.SSLMode)
}

type Config struct {
	Port int
	DB   DBConfig

	// StorageDir is where LocalFileBackend writes encrypted document blobs.
	StorageDir string
	// StorageMasterKeyHex is the AES-256 key (hex, 32 bytes) for encryption
	// at rest — same posture as secret-vault-integration-svc's master key.
	StorageMasterKeyHex string

	// TenantRegistryURL is tenant-entity-registry-svc's base URL, used for
	// the residency check (GET /v1/tenants/{id}/residency-region).
	TenantRegistryURL string
}

func Load() (*Config, error) {
	port, err := intEnv("PORT", 8094)
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
			Name:     strEnv("DB_NAME", "document_vault"),
			User:     strEnv("DB_USER", "postgres"),
			Password: strEnv("DB_PASSWORD", ""),
			SSLMode:  strEnv("DB_SSLMODE", "disable"),
		},
		StorageDir:          strEnv("STORAGE_DIR", "./data/documents"),
		StorageMasterKeyHex: strEnv("DOCUMENT_VAULT_MASTER_KEY_HEX", ""),
		TenantRegistryURL:   strEnv("TENANT_REGISTRY_URL", "http://tenant-svc:8081"),
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
