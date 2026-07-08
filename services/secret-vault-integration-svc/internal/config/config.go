package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration for secret-vault-integration-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	// VaultKeyPath is where the v1 LocalFileVaultBackend persists its
	// encrypted-at-rest secret material. Production replaces this whole
	// backend with a real HashiCorp Vault / cloud KMS client — see
	// context.md §7.6. This is the same class of local-file bootstrap
	// compromise identity-context-svc's JWT_SIGNING_PRIVATE_KEY_PATH
	// already accepts.
	VaultKeyPath string

	// VaultMasterKeyHex is the AES-256 key (32 bytes, hex-encoded) used
	// to encrypt/decrypt secret material at rest. Never has a default —
	// must be supplied, or the backend refuses to start.
	VaultMasterKeyHex string
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
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8087),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "secret_vault_integration"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		VaultKeyPath:      env("VAULT_LOCAL_STORE_PATH", "./secret_store.local"),
		VaultMasterKeyHex: env("VAULT_MASTER_KEY_HEX", ""),
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
