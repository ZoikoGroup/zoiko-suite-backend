package config_test

import (
	"os"
	"testing"

	"zoiko.io/workflow-svc/internal/config"
)

func TestLoad_Defaults(t *testing.T) {
	// Clear env vars that might affect test
	os.Unsetenv("ENV")
	os.Unsetenv("PORT")
	os.Unsetenv("DB_HOST")
	os.Unsetenv("DB_PORT")
	os.Unsetenv("DB_NAME")
	os.Unsetenv("DB_USER")
	os.Unsetenv("DB_PASSWORD")
	os.Unsetenv("DB_SSLMODE")
	os.Unsetenv("KAFKA_BROKERS")
	os.Unsetenv("KAFKA_GROUP_ID")
	os.Unsetenv("KAFKA_EVENTS_TOPIC")
	os.Unsetenv("AUTHORIZATION_SERVICE_URL")
	os.Unsetenv("DOCUMENT_VAULT_SERVICE_URL")
	os.Unsetenv("GOVERNANCE_DECISION_LOG_SERVICE_URL")
	os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Env != "local" {
		t.Errorf("Env = %q, want local", cfg.Env)
	}
	if cfg.Port != 8090 {
		t.Errorf("Port = %d, want 8090", cfg.Port)
	}
	if cfg.DB.Host != "localhost" {
		t.Errorf("DB.Host = %q, want localhost", cfg.DB.Host)
	}
	if cfg.DB.Port != 5432 {
		t.Errorf("DB.Port = %d, want 5432", cfg.DB.Port)
	}
	if cfg.DB.Name != "workflow" {
		t.Errorf("DB.Name = %q, want workflow", cfg.DB.Name)
	}
	if cfg.DB.User != "postgres" {
		t.Errorf("DB.User = %q, want postgres", cfg.DB.User)
	}
	if cfg.DB.SSLMode != "require" {
		t.Errorf("DB.SSLMode = %q, want require", cfg.DB.SSLMode)
	}
	if len(cfg.Kafka.Brokers) != 1 || cfg.Kafka.Brokers[0] != "localhost:9092" {
		t.Errorf("Kafka.Brokers = %v, want [localhost:9092]", cfg.Kafka.Brokers)
	}
	if cfg.Kafka.GroupID != "workflow-svc" {
		t.Errorf("Kafka.GroupID = %q, want workflow-svc", cfg.Kafka.GroupID)
	}
	if cfg.Kafka.Topic != "zoiko.workflow.events" {
		t.Errorf("Kafka.Topic = %q, want zoiko.workflow.events", cfg.Kafka.Topic)
	}
	if cfg.AuthorizationServiceURL != "http://authorization-svc:8089" {
		t.Errorf("AuthorizationServiceURL = %q, want http://authorization-svc:8089", cfg.AuthorizationServiceURL)
	}
	if cfg.DocumentVaultServiceURL != "http://document-vault-svc:8094" {
		t.Errorf("DocumentVaultServiceURL = %q, want http://document-vault-svc:8094", cfg.DocumentVaultServiceURL)
	}
	if cfg.GovernanceDecisionLogServiceURL != "http://governance-decision-log-svc:8083" {
		t.Errorf("GovernanceDecisionLogServiceURL = %q, want http://governance-decision-log-svc:8083", cfg.GovernanceDecisionLogServiceURL)
	}
	if cfg.OTELExporterEndpoint != "http://otel-collector:4318" {
		t.Errorf("OTELExporterEndpoint = %q, want http://otel-collector:4318", cfg.OTELExporterEndpoint)
	}
}

func TestLoad_EnvironmentOverrides(t *testing.T) {
	os.Setenv("ENV", "production")
	os.Setenv("PORT", "9090")
	os.Setenv("DB_HOST", "db.example.com")
	os.Setenv("DB_PORT", "5433")
	os.Setenv("DB_NAME", "custom_db")
	os.Setenv("DB_USER", "custom_user")
	os.Setenv("DB_PASSWORD", "secret")
	os.Setenv("DB_SSLMODE", "disable")
	os.Setenv("KAFKA_BROKERS", "kafka1:9092,kafka2:9092")
	os.Setenv("KAFKA_GROUP_ID", "custom-group")
	os.Setenv("KAFKA_EVENTS_TOPIC", "custom.topic")
	os.Setenv("AUTHORIZATION_SERVICE_URL", "http://auth:8089")
	os.Setenv("DOCUMENT_VAULT_SERVICE_URL", "http://vault:8094")
	os.Setenv("GOVERNANCE_DECISION_LOG_SERVICE_URL", "http://decision:8083")
	os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel:4318")
	defer func() {
		os.Unsetenv("ENV")
		os.Unsetenv("PORT")
		os.Unsetenv("DB_HOST")
		os.Unsetenv("DB_PORT")
		os.Unsetenv("DB_NAME")
		os.Unsetenv("DB_USER")
		os.Unsetenv("DB_PASSWORD")
		os.Unsetenv("DB_SSLMODE")
		os.Unsetenv("KAFKA_BROKERS")
		os.Unsetenv("KAFKA_GROUP_ID")
		os.Unsetenv("KAFKA_EVENTS_TOPIC")
		os.Unsetenv("AUTHORIZATION_SERVICE_URL")
		os.Unsetenv("DOCUMENT_VAULT_SERVICE_URL")
		os.Unsetenv("GOVERNANCE_DECISION_LOG_SERVICE_URL")
		os.Unsetenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.Env != "production" {
		t.Errorf("Env = %q, want production", cfg.Env)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.DB.Host != "db.example.com" {
		t.Errorf("DB.Host = %q, want db.example.com", cfg.DB.Host)
	}
	if cfg.DB.Port != 5433 {
		t.Errorf("DB.Port = %d, want 5433", cfg.DB.Port)
	}
	if cfg.DB.Name != "custom_db" {
		t.Errorf("DB.Name = %q, want custom_db", cfg.DB.Name)
	}
	if cfg.DB.User != "custom_user" {
		t.Errorf("DB.User = %q, want custom_user", cfg.DB.User)
	}
	if cfg.DB.Password != "secret" {
		t.Errorf("DB.Password = %q, want secret", cfg.DB.Password)
	}
	if cfg.DB.SSLMode != "disable" {
		t.Errorf("DB.SSLMode = %q, want disable", cfg.DB.SSLMode)
	}
	if len(cfg.Kafka.Brokers) != 2 || cfg.Kafka.Brokers[0] != "kafka1:9092" || cfg.Kafka.Brokers[1] != "kafka2:9092" {
		t.Errorf("Kafka.Brokers = %v, want [kafka1:9092 kafka2:9092]", cfg.Kafka.Brokers)
	}
	if cfg.Kafka.GroupID != "custom-group" {
		t.Errorf("Kafka.GroupID = %q, want custom-group", cfg.Kafka.GroupID)
	}
	if cfg.Kafka.Topic != "custom.topic" {
		t.Errorf("Kafka.Topic = %q, want custom.topic", cfg.Kafka.Topic)
	}
	if cfg.AuthorizationServiceURL != "http://auth:8089" {
		t.Errorf("AuthorizationServiceURL = %q, want http://auth:8089", cfg.AuthorizationServiceURL)
	}
	if cfg.DocumentVaultServiceURL != "http://vault:8094" {
		t.Errorf("DocumentVaultServiceURL = %q, want http://vault:8094", cfg.DocumentVaultServiceURL)
	}
	if cfg.GovernanceDecisionLogServiceURL != "http://decision:8083" {
		t.Errorf("GovernanceDecisionLogServiceURL = %q, want http://decision:8083", cfg.GovernanceDecisionLogServiceURL)
	}
	if cfg.OTELExporterEndpoint != "http://otel:4318" {
		t.Errorf("OTELExporterEndpoint = %q, want http://otel:4318", cfg.OTELExporterEndpoint)
	}
}

func TestDBConfig_DSN(t *testing.T) {
	db := config.DBConfig{
		Host:     "localhost",
		Port:     5432,
		Name:     "testdb",
		User:     "testuser",
		Password: "testpass",
		SSLMode:  "require",
	}

	dsn := db.DSN()
	expected := "host=localhost port=5432 dbname=testdb user=testuser password=testpass sslmode=require"
	if dsn != expected {
		t.Errorf("DSN() = %q, want %q", dsn, expected)
	}
}

func TestEnvInt_InvalidValue(t *testing.T) {
	os.Setenv("TEST_INT", "not-a-number")
	defer os.Unsetenv("TEST_INT")

	// This tests the internal envInt function indirectly through Load
	// We can't directly test envInt since it's unexported, but we can verify
	// that invalid PORT falls back to default
	os.Setenv("PORT", "not-a-number")
	defer os.Unsetenv("PORT")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	// Should fall back to default 8090
	if cfg.Port != 8090 {
		t.Errorf("Port = %d, want 8090 (fallback)", cfg.Port)
	}
}
