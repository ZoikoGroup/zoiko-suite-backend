package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"zoiko.io/notification-svc/internal/quota"
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

	// ActionTokenSecret is the HMAC-SHA256 signing key for action link tokens
	// (ZS-COMMS-EMAIL-001 §6). Must be at least 16 bytes.
	// Sourced from ACTION_TOKEN_SECRET. An empty value disables the action
	// gateway — tokens cannot be generated or verified without it.
	ActionTokenSecret string

	// WebhookSecrets are the per-provider HMAC secrets that authenticate provider
	// callbacks (ZS-SVC-Y-001 INV-27). Sourced from NOTIFICATION_WEBHOOK_SECRETS,
	// a JSON object of provider name to a list of secrets (several so a secret can
	// be rotated): {"ses":["new-secret-......","old-secret-......"]}. Each secret
	// must be at least 16 bytes. A provider with no entry has every callback
	// refused; an empty value therefore closes the webhook ingress entirely.
	// LedgerRegisterEnabled makes every ledger-pipeline delivery also a register
	// row (the notifications table) linked to its intent, so one communication has
	// one identity (ZS-SVC-Y-001 INV-02; plan step 3). Sourced from
	// NOTIFICATION_LEDGER_REGISTER_ENABLED, default false: it makes the pipeline
	// write twice and changes where ledger deliveries are visible, so it is turned
	// on deliberately, per environment.
	LedgerRegisterEnabled bool

	// PrivacyEnforcement makes the direct send path ask privacy-decision-svc before an
	// intent-bound email goes out, and fail closed when the answer is anything but PERMIT
	// (ZS-SVC-Y-001 INV-30, NP-17). NOTIFICATION_PRIVACY_ENFORCEMENT, default false: it
	// starts refusing sends whose intent has no privacy binding, so it is turned on
	// deliberately, once intents carry one. It needs PRIVACY_DECISION_URL.
	PrivacyEnforcement bool
	PrivacyDecisionURL string
	PrivacyTimeout     time.Duration

	// QuotaEnabled turns on the send quotas (ZS-SVC-Y-001 6.5): per tenant, recipient and intent,
	// with protected capacity for security messages. NOTIFICATION_QUOTA_ENABLED, default false,
	// because the right limits depend on the largest legitimate batch an environment sends (a
	// payroll run, say), so it is turned on deliberately with limits that fit. Each limit has its
	// own variable; zero removes that budget.
	QuotaEnabled bool
	QuotaLimits  quota.Limits

	WebhookSecrets map[string][]string
	// WebhookTolerance is how far a callback timestamp may differ from the clock.
	// Sourced from NOTIFICATION_WEBHOOK_TOLERANCE (default 5m).
	WebhookTolerance time.Duration

	// SecondaryEmail is an optional failover SMTP provider. When configured,
	// the router fails over to it after a transient primary failure (§13 P1-12).
	// All secondary vars default to empty (disabled).
	SecondaryEmail EmailConfig

	// WebhookDLQ configures the periodic background worker that reprocesses
	// retryable webhook DLQ records.
	WebhookDLQ WebhookDLQConfig
}

type WebhookDLQConfig struct {
	Enabled   bool
	Interval  time.Duration
	BatchSize int
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

	// StrandedAfter is how long a notification may sit in flight — PENDING
	// with nothing scheduled — before the worker treats it as abandoned and
	// puts it back on the schedule.
	//
	// It must exceed the longest attempt this service can make, or the sweep
	// could reschedule a send another replica is still working on and the
	// recipient gets the notice twice. The SMTP provider's own timeout
	// defaults to 10s and the HTTP server's WriteTimeout is 15s, so a real
	// attempt cannot outlive roughly 30 seconds; 15 minutes is generous
	// headroom that still recovers a stranded notice the same hour rather
	// than never.
	//
	// Zero disables the sweep and is a true off switch, not a "sweep
	// everything immediately" — which is the dangerous reading of 0 here, and
	// the reason it is handled explicitly rather than falling through to a
	// default.
	StrandedAfter time.Duration
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

// ActionLinkBaseURL returns the externally-reachable base URL for the action
// link gateway. Set ACTION_LINK_BASE_URL explicitly when the service is behind
// a reverse proxy with a different hostname than its listen address.
// If unset, an empty string is returned and the Signer generates relative URLs.
func (c *Config) ActionLinkBaseURL() string {
	return env("ACTION_LINK_BASE_URL", "")
}

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
			// Deliberately NOT gated on Enabled. Retry being switched off
			// means "do not re-attempt a failed delivery", and the handler
			// concludes those as FAILED so none of them sits in flight. A
			// stranded row is a different thing — an attempt that never got
			// to report any outcome at all — and abandoning it because
			// retries are off would leave the exact silent non-delivery this
			// sweep exists to end.
			StrandedAfter: envDuration("NOTIFICATION_STRANDED_AFTER", 15*time.Minute),
		},

		AuthzMTLSEnabled:         env("AUTHZ_MTLS_ENABLED", "false") == "true",
		AuthzMTLSURL:             env("AUTHZ_MTLS_URL", "https://authorization-svc:8449"),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),

		ActionTokenSecret: env("ACTION_TOKEN_SECRET", ""),

		WebhookTolerance: envDuration("NOTIFICATION_WEBHOOK_TOLERANCE", 5*time.Minute),

		LedgerRegisterEnabled: env("NOTIFICATION_LEDGER_REGISTER_ENABLED", "false") == "true",

		PrivacyEnforcement: env("NOTIFICATION_PRIVACY_ENFORCEMENT", "false") == "true",
		PrivacyDecisionURL: env("PRIVACY_DECISION_URL", ""),
		PrivacyTimeout:     envDuration("PRIVACY_DECISION_TIMEOUT", 3*time.Second),

		QuotaEnabled: env("NOTIFICATION_QUOTA_ENABLED", "false") == "true",
		QuotaLimits: quota.Limits{
			TenantPerMinute:    envInt("NOTIFICATION_QUOTA_TENANT_PER_MINUTE", quota.DefaultLimits.TenantPerMinute),
			TenantS0PerMinute:  envInt("NOTIFICATION_QUOTA_TENANT_S0_PER_MINUTE", quota.DefaultLimits.TenantS0PerMinute),
			RecipientPerHour:   envInt("NOTIFICATION_QUOTA_RECIPIENT_PER_HOUR", quota.DefaultLimits.RecipientPerHour),
			RecipientS0PerHour: envInt("NOTIFICATION_QUOTA_RECIPIENT_S0_PER_HOUR", quota.DefaultLimits.RecipientS0PerHour),
			IntentPerMinute:    envInt("NOTIFICATION_QUOTA_INTENT_PER_MINUTE", quota.DefaultLimits.IntentPerMinute),
		},

		SecondaryEmail: EmailConfig{
			Provider:       env("SMTP_SECONDARY_PROVIDER", ""),
			Host:           env("SMTP_SECONDARY_HOST", ""),
			Port:           envInt("SMTP_SECONDARY_PORT", 587),
			Username:       env("SMTP_SECONDARY_USERNAME", ""),
			Password:       env("SMTP_SECONDARY_PASSWORD", ""),
			From:           env("SMTP_SECONDARY_FROM", ""),
			TLSMode:        env("SMTP_SECONDARY_TLS_MODE", "starttls"),
			AllowCleartext: env("SMTP_SECONDARY_ALLOW_CLEARTEXT", "false") == "true",
			VerifyOnStart:  env("SMTP_SECONDARY_VERIFY_ON_START", "true") == "true",
		},

		WebhookDLQ: WebhookDLQConfig{
			Enabled:   env("NOTIFICATION_WEBHOOK_DLQ_ENABLED", "true") == "true",
			Interval:  envDuration("NOTIFICATION_WEBHOOK_DLQ_INTERVAL", 1*time.Minute),
			BatchSize: envInt("NOTIFICATION_WEBHOOK_DLQ_BATCH_SIZE", 50),
		},
	}

	// A malformed secret list is refused outright rather than ignored: ignoring it
	// would close the webhook ingress (every callback refused) with nothing to say
	// why, and a short secret would silently weaken the signature.
	secrets, err := parseWebhookSecrets(os.Getenv("NOTIFICATION_WEBHOOK_SECRETS"))
	if err != nil {
		return nil, err
	}
	cfg.WebhookSecrets = secrets

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
	// Enforcement without an authority to ask would refuse every send, silently.
	if cfg.PrivacyEnforcement && cfg.PrivacyDecisionURL == "" {
		return nil, errors.New("NOTIFICATION_PRIVACY_ENFORCEMENT is on but PRIVACY_DECISION_URL is not set")
	}
	return cfg, nil
}

// minWebhookSecret mirrors webhook.MinSecretLength; config does not import the
// webhook package, so the number is repeated and a test pins them together.
const minWebhookSecret = 16

// parseWebhookSecrets reads NOTIFICATION_WEBHOOK_SECRETS. Empty is valid and means
// "no provider may call back".
func parseWebhookSecrets(raw string) (map[string][]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out map[string][]string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("invalid NOTIFICATION_WEBHOOK_SECRETS: must be a JSON object of provider to a list of secrets: %w", err)
	}
	for provider, list := range out {
		if strings.TrimSpace(provider) == "" {
			return nil, errors.New("invalid NOTIFICATION_WEBHOOK_SECRETS: empty provider name")
		}
		if len(list) == 0 {
			return nil, fmt.Errorf("invalid NOTIFICATION_WEBHOOK_SECRETS: provider %q has no secrets", provider)
		}
		for _, s := range list {
			if len(s) < minWebhookSecret {
				return nil, fmt.Errorf("invalid NOTIFICATION_WEBHOOK_SECRETS: a secret for provider %q is shorter than %d bytes", provider, minWebhookSecret)
			}
		}
	}
	return out, nil
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
