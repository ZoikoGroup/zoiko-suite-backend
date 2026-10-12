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

	AuthZServiceURL string

	// ServicePrincipalID is this service's identity in authorization-svc, used
	// to EXECUTE a review's REVOKE decision (it holds iam.assignment.revoke);
	// the reviewer is recorded as the decider. Empty executes as the reviewer.
	ServicePrincipalID string

	// HR lifecycle events (S9-C2): the topics the event-triggered review
	// consumer reads, its consumer group, the default reviewer of an event
	// review, and how long such a review stays open. An empty reviewer
	// leaves the consumer off.
	HREventTopics       []string
	HREventGroupID      string
	EventReviewReviewer string
	EventReviewDueDays  int

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
}

func (d DBConfig) DSN() string {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
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

func Load() (*Config, error) {
	return &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8137),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "access_control"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "access-control-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.access-control.events"),
		},
		AuthZServiceURL: env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		ServicePrincipalID: env("ACS_SERVICE_PRINCIPAL_ID", "svc-access-control"),
		HREventTopics:       strings.Split(env("ACS_HR_EVENT_TOPICS", "zoiko.employee.events,zoiko.offboarding.events"), ","),
		HREventGroupID:      env("ACS_HR_EVENT_GROUP_ID", "access-control-svc-hr-reviews"),
		EventReviewReviewer: env("ACS_EVENT_REVIEW_DEFAULT_REVIEWER", ""),
		EventReviewDueDays:  envInt("ACS_EVENT_REVIEW_DUE_DAYS", 7),

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
