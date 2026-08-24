package config

import (
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for authorization-svc.
type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	// JurisdictionRulesURL is used only when creating a jurisdiction-scoped
	// SoD rule — see internal/jurisdiction.HTTPValidator.
	JurisdictionRulesURL string

	// OTELExporterEndpoint is where internal/telemetry sends OTLP/HTTP
	// traces (03-microservices.md §3.8's Observability Baseline).
	OTELExporterEndpoint string

	// MTLS fields wire the material-path mTLS pilot (docs/original_doc/
	// zoiko_suite_doc5.txt:76,251 — mTLS mandated for "material paths", not
	// every call). Disabled by default: MTLSEnabled=false leaves every
	// existing caller on the plain HTTP port untouched. When enabled, this
	// service ALSO listens on MTLSPort with a server certificate obtained
	// from mtls-management-svc, requiring a verified client certificate
	// signed by the same CA — the plain port keeps running alongside it, so
	// turning this on never breaks a caller that hasn't migrated yet.
	MTLSEnabled              bool
	MTLSPort                 int
	MTLSManagementServiceURL string

	// SIEMServiceURL is siem-integration-svc. Empty disables streaming —
	// see internal/siem's doc comment.
	SIEMServiceURL string

	// PlatformScopeEntityID is the synthetic legal_entity_id a platform-wide
	// governance act is authorized against — the same pattern policy-svc uses
	// for a policy with no owning legal entity. Creating a segregation-of-
	// duties rule with no tenant_id applies it to every tenant, so it is
	// authorized against this entity rather than the caller's own tenant
	// scope (handler.requirePlatformAction).
	//
	// Empty by default and empty means REFUSE, not permit: a deployment that
	// has not provisioned the platform-scope entity and its role grant cannot
	// author platform-wide rules at all. That is the safe direction for a
	// control whose blast radius is the whole estate.
	PlatformScopeEntityID string
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
		// 8089: 8080-8088 are already taken by every other Phase 1 service
		// built so far — see services/README.md.
		Port: envInt("PORT", 8089),
		// DB_NAME defaults to "authorization_svc", not "authorization" —
		// AUTHORIZATION is a reserved SQL keyword (CREATE SCHEMA ...
		// AUTHORIZATION owner), so a bare CREATE DATABASE authorization
		// fails with a syntax error. Avoids needing to quote the
		// identifier everywhere, forever.
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "authorization_svc"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "authorization-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.authorization.events"),
		},
		JurisdictionRulesURL: env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8082"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		MTLSEnabled:              envBool("MTLS_ENABLED", false),
		MTLSPort:                 envInt("MTLS_PORT", 8449),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		SIEMServiceURL:           env("SIEM_SERVICE_URL", ""),
		PlatformScopeEntityID:    env("AUTHZ_PLATFORM_SCOPE_ENTITY_ID", ""),
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

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
