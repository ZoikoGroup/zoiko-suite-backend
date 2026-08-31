package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	EmployeeMasterURL string
	AuthZServiceURL   string

	// AuthzMTLSEnabled/AuthzMTLSURL wire this service into the material-path
	// mTLS pilot (see authorization-svc/internal/mtls's doc comment).
	// Disabled by default — AuthZServiceURL (plain HTTP) keeps being used
	// unless explicitly turned on.
	AuthzMTLSEnabled         bool
	AuthzMTLSURL             string
	MTLSManagementServiceURL string

	OTELExporterEndpoint string
}

type DBConfig struct {
	Host     string
	Port     int
	Name     string
	User     string
	Password string
	SSLMode  string

	// Schema is the Postgres schema holding this service's tables, applied to
	// the connection as search_path.
	//
	// Empty means the server default, which is what the database-per-service
	// compose estate uses: the service owns a whole database and its tables sit
	// in that database's public schema.
	//
	// A managed single-database host cannot express the 63 databases
	// deployments/init-db.sh creates, so each service gets a schema instead. The
	// migrations need no change - they say CREATE TABLE, which lands wherever
	// search_path points.
	//
	// A schema named here that does not exist is NOT a connect-time error:
	// Postgres drops unresolvable entries from search_path silently, so the
	// failure surfaces on the first query as "relation ... does not exist".
	// Check the schema exists and that DB_USER holds USAGE on it.
	Schema string

	// Options is appended to the DSN verbatim, for pgx settings that vary by
	// deployment rather than by service.
	//
	// The case this exists for is connection pooling. pgx defaults to cached
	// NAMED prepared statements, which PgBouncer in transaction mode breaks: the
	// statement is prepared on one server connection and executed on another.
	// The error surfaces only under concurrency, so it passes every smoke test
	// and then fails in production.
	//
	//	DB_OPTIONS="default_query_exec_mode=exec statement_cache_capacity=0"
	//
	// Leave empty when connecting to a database directly.
	Options string
}

func (d DBConfig) DSN() string {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	dsn := "host=" + d.Host +
		" port=" + strconv.Itoa(d.Port) +
		" dbname=" + d.Name +
		" user=" + d.User +
		" password=" + quoteDSNValue(d.Password) +
		" sslmode=" + d.SSLMode
	if d.Schema != "" {
		dsn += " search_path=" + d.Schema
	}
	if d.Options != "" {
		dsn += " " + d.Options
	}
	return dsn
}

// quoteDSNValue renders v as a single-quoted keyword/value DSN literal.
//
// The password was interpolated bare, which silently produces a malformed DSN
// for any value containing a space or a quote - and a managed host generates
// exactly those. pgx then reports a parse or authentication failure that points
// at the credential rather than at the encoding of it.
func quoteDSNValue(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(v); i++ {
		if c := v[i]; c == '\'' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('\'')
	return b.String()
}

type KafkaConfig struct {
	Brokers []string
	GroupID string
	Topic   string
}

func Load() (*Config, error) {
	return &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8115),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "leave_absence"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
			Schema:   env("DB_SCHEMA", ""),
			Options:  env("DB_OPTIONS", ""),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "leave-absence-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.leaveabsence.events"),
		},
		EmployeeMasterURL: env("EMPLOYEE_MASTER_URL", "http://employee-master-svc:8108"),
		AuthZServiceURL:   env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),
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
