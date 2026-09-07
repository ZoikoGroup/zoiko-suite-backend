package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env  string
	Port int

	DB DBConfig

	Kafka KafkaConfig

	AuthZServiceURL string

	// IdentityServiceURL is identity-context-svc, which owns principals and
	// their contact facts. Recipient resolution goes there; without it, an
	// EMAIL notification has no address to be delivered to.
	IdentityServiceURL string

	Email EmailConfig

	Retry RetryConfig

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

// DSN builds the connection string from the DB_* variables and nothing else.
//
// It used to return TEST_DATABASE_URL when that variable was set, "for
// integration tests" — but no test calls this function (the store suite opens
// its own pool from that variable directly), so all it did was give the
// running service a way to silently connect somewhere other than its
// configured database if the variable ever leaked into an environment. The
// store suite DROPs tables. Same removal as financial-close-svc.
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

// RetryConfig bounds the re-attempt of transient delivery failures.
//
// Every value is bounded by the retry package's own Normalize, so a
// misconfigured deployment degrades to the default rather than turning into an
// unbounded resend loop against somebody's mail server.
type RetryConfig struct {
	// Enabled false pins MaxAttempts to 1, which the handler reports on the
	// record as "retry is disabled by configuration" rather than leaving a
	// notification PENDING with nothing scheduled to move it.
	Enabled bool

	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration

	// Interval is how often the worker polls for due retries, and BatchSize
	// how many it takes per poll.
	Interval  time.Duration
	BatchSize int
}

// EmailConfig describes the outbound mail provider.
//
// Provider is empty by default, and an empty Provider means EMAIL delivery is
// not configured. That is a deliberate default rather than an oversight: a
// service that invents a mail server it can reach would send real mail from a
// developer's laptop the first time someone ran it, and the previous behaviour
// — reporting success with no provider at all — is the failure this whole
// change exists to remove. Unconfigured EMAIL now produces a FAILED record
// naming the missing provider, which is true and visible.
type EmailConfig struct {
	// Provider selects the transport. "smtp" is the only implementation;
	// "" disables email delivery.
	Provider string

	Host     string
	Port     int
	Username string
	Password string

	// From is the envelope sender and From: header, RFC 5322 — either a bare
	// address or "Display Name <address>".
	From string

	// TLSMode is starttls (default), implicit, or none. "none" is rejected for
	// any non-loopback host by the provider unless AllowCleartext is set, and
	// rejected outright in production below.
	TLSMode string

	// AllowCleartext permits TLSMode "none" against a non-loopback host — a
	// mail catcher running as a sibling container. Never in production.
	AllowCleartext bool

	// VerifyOnStart opens an SMTP session at startup — connect, STARTTLS,
	// AUTH, NOOP, QUIT — to prove the credentials before any notification
	// depends on them. No mail is sent and nothing counts against a provider
	// quota.
	//
	// On by default. A new SMTP credential is pasted into an environment and
	// then not exercised until something real needs it, which is the worst
	// possible moment to discover a typo. Set false where the relay is
	// deliberately unreachable from where the service starts.
	VerifyOnStart bool
}

// Configured reports whether a mail provider is set up.
func (e EmailConfig) Configured() bool { return e.Provider != "" }

func Load() (*Config, error) {
	cfg := &Config{
		Env:  env("ENV", "local"),
		Port: envInt("PORT", 8133),
		DB: DBConfig{
			Host:     env("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			Name:     env("DB_NAME", "notification"),
			User:     env("DB_USER", "postgres"),
			Password: env("DB_PASSWORD", ""),
			SSLMode:  env("DB_SSLMODE", "require"),
		},
		Kafka: KafkaConfig{
			// os.LookupEnv, not env(): KAFKA_BROKERS= (explicitly empty) is how
			// a deployment says "no broker — publish in dry mode". env() would
			// substitute the default and make that unreachable.
			Brokers: brokers(),
			GroupID: env("KAFKA_GROUP_ID", "notification-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.notification.events"),
		},
		AuthZServiceURL:      env("AUTHZ_SERVICE_URL", "http://authorization-svc:8089"),
		IdentityServiceURL:   env("IDENTITY_SERVICE_URL", "http://identity-context-svc:8080"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		Email: EmailConfig{
			Provider: env("NOTIFICATION_EMAIL_PROVIDER", ""),
			Host:     env("SMTP_HOST", ""),
			Port:     envInt("SMTP_PORT", 587),
			Username: env("SMTP_USERNAME", ""),
			Password: env("SMTP_PASSWORD", ""),
			From:     env("NOTIFICATION_EMAIL_FROM", ""),
			TLSMode:  env("SMTP_TLS_MODE", "starttls"),

			AllowCleartext: env("SMTP_ALLOW_CLEARTEXT", "false") == "true",
			VerifyOnStart:  env("SMTP_VERIFY_ON_START", "true") == "true",
		},

		Retry: RetryConfig{
			Enabled:     env("NOTIFICATION_RETRY_ENABLED", "true") == "true",
			MaxAttempts: envInt("NOTIFICATION_RETRY_MAX_ATTEMPTS", 5),
			BaseDelay:   envDuration("NOTIFICATION_RETRY_BASE_DELAY", 30*time.Second),
			MaxDelay:    envDuration("NOTIFICATION_RETRY_MAX_DELAY", 8*time.Minute),
			Interval:    envDuration("NOTIFICATION_RETRY_INTERVAL", 10*time.Second),
			BatchSize:   envInt("NOTIFICATION_RETRY_BATCH_SIZE", 50),
		},

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
	}

	// Load returned a nil error unconditionally, so every default above was
	// also a production default: an empty DB password, and an authz URL that
	// could point at a placeholder. Refuse to start rather than run a
	// governed service on them.
	if cfg.Env == "production" {
		var missing []string
		if cfg.DB.Password == "" {
			missing = append(missing, "DB_PASSWORD")
		}
		if cfg.DB.SSLMode == "disable" {
			missing = append(missing, "DB_SSLMODE (must not be 'disable' in production)")
		}
		if cfg.AuthZServiceURL == "" || strings.Contains(cfg.AuthZServiceURL, "localhost") {
			missing = append(missing, "AUTHZ_SERVICE_URL (must be a real authorization-svc address)")
		}
		if cfg.IdentityServiceURL == "" || strings.Contains(cfg.IdentityServiceURL, "localhost") {
			missing = append(missing, "IDENTITY_SERVICE_URL (must be a real identity-context-svc address)")
		}

		// An unconfigured mail provider in production is not a silent default.
		// Every EMAIL notification would be recorded FAILED, which is at least
		// honest, but it is a state nobody intends and one that only becomes
		// visible when a password reset does not arrive.
		if !cfg.Email.Configured() {
			missing = append(missing, "NOTIFICATION_EMAIL_PROVIDER (EMAIL delivery would fail for every notification)")
		} else {
			if cfg.Email.From == "" {
				missing = append(missing, "NOTIFICATION_EMAIL_FROM")
			}
			if cfg.Email.Host == "" {
				missing = append(missing, "SMTP_HOST")
			}
			// The provider already refuses this for a non-loopback host. Named
			// here as well because in production the answer is no regardless
			// of host: a loopback relay on a production node still forwards
			// onward, and the operator who set this meant to reach it.
			if cfg.Email.TLSMode == "none" {
				missing = append(missing, "SMTP_TLS_MODE (must not be 'none' in production — "+
					"password resets carry a temporary password and would cross the network in the clear)")
			}
			// Named separately so the message says which variable to remove.
			// A production deployment that sets this has almost certainly
			// inherited a developer's compose environment wholesale.
			if cfg.Email.AllowCleartext {
				missing = append(missing, "SMTP_ALLOW_CLEARTEXT (must not be set in production; "+
					"it exists for a local mail catcher on a container network)")
			}
		}

		if len(missing) > 0 {
			return nil, errors.New("invalid production config: " + strings.Join(missing, ", "))
		}
	}
	return cfg, nil
}

func brokers() []string {
	v, ok := os.LookupEnv("KAFKA_BROKERS")
	if !ok {
		return []string{"localhost:9092"}
	}
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return strings.Split(v, ",")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envDuration reads a Go duration string ("30s", "8m").
//
// An unparseable value falls back to the default rather than failing to start,
// matching envInt above — but it is logged nowhere, so the deployment that set
// NOTIFICATION_RETRY_BASE_DELAY=30 (no unit) gets 30 seconds by luck of the
// default rather than 30 nanoseconds by parse. That is the safer of the two
// silent outcomes, which is why the default is not simply zero.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
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
