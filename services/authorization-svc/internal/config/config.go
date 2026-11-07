package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
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
	// MTLSBootstrapTokenPath points at the shared self-provisioning secret
	// (see deployments/docker-compose.yml's mtls-bootstrap-keygen). Empty
	// means this service presents no bootstrap token and falls back to
	// mtls-management-svc's normal principal/authorize path (which will
	// fail, since this service has no principal at startup) — MTLSEnabled
	// should not be turned on without this also being set.
	MTLSBootstrapTokenPath string
	// MTLSRevocationCheck is "require" (default) or "off": whether the mTLS
	// listener refuses client certificates on mtls-management-svc's CRL, and
	// refuses every client certificate while it holds no current CRL
	// (register M-03). "off" is accepted only with ENV=local.
	MTLSRevocationCheck string
	// MTLSCRLURL is where the CRL is fetched (default: the management
	// service's /v1/mtls/crl); MTLSCRLRefreshSeconds how often.
	MTLSCRLURL            string
	MTLSCRLRefreshSeconds int

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
	//
	// It is also what handler.PlatformScopeSentinel resolves to, so a caller
	// asking for a platform-wide authorization decision
	// (legal_entity_id=PLATFORM) is evaluated against the same id this
	// service uses for its own platform-wide acts — one id, configured once,
	// instead of each calling service inventing its own synthetic uuid
	// (tracker item 67).
	PlatformScopeEntityID string

	// CacheTTL bounds how long an evaluation read may be served from memory
	// on the /v1/authorize path. See internal/cache's package comment for
	// what is cached (the four evaluation reads plus the ABAC rule lookup)
	// and what deliberately is not (the decision, and the decision-log
	// insert, on every path).
	//
	// This is an in-process cache, so it is also the bound on how long a
	// grant revoked through ANOTHER replica can still authorize. Five seconds
	// by default, deliberately short. AUTHZ_CACHE_TTL_SECONDS=0 disables caching
	// entirely — a real off switch, not a zero-second TTL — for a deployment
	// that cannot accept that window.
	CacheTTL time.Duration

	// AccessDecisionRetentionMonths is how many whole months of decision
	// artifacts the hot access_decision_log partitions keep, for the
	// operational tooling that calls
	// detach_access_decision_log_partitions_before(). 0 means never detach.
	//
	// The service itself never deletes or detaches anything — see migration
	// 000009. This value exists so the retention window is declared next to
	// the rest of the service's configuration rather than living only in
	// whatever cron invokes the function.
	AccessDecisionRetentionMonths int
	RetentionSweepInterval        time.Duration
	PartitionMonthsAhead          int

	// OutboxRetention / IdempotencyRetention: how long the retention sweep
	// keeps a published outbox row and an Idempotency-Key record (000031,
	// Governance audit N-14). 0 keeps them forever, the old behaviour.
	OutboxRetention      time.Duration
	IdempotencyRetention time.Duration

	// EnforceTenantOnAuthorize controls whether /v1/authorize requires a tenant
	// scope (via X-Tenant-Id header or tenant_id in body). When false (default),
	// tenantless requests are allowed but logged with a warning — they are
	// evaluated against globally-applicable SoD rules only. When true, a missing
	// tenant scope returns 400. This is a migration aid for the ~86 callers that
	// do not yet forward X-Tenant-Id; once they are all migrated, this should be
	// set to true.
	EnforceTenantOnAuthorize bool

	// EntityRegistryURL is tenant-entity-registry-svc. /v1/authorize asks it
	// whether an entity the projection has not heard of is in the claimed
	// tenant (or, with no claim, in one the principal holds authority in), so
	// no evaluation is scoped by an unverified tenant or run across tenants.
	// Empty disables the check and keeps the projection-only behaviour.
	EntityRegistryURL string

	// CommandContractEnforce refuses a privileged or destructive admin
	// command without purpose / reason_code, and a delegation without an end
	// date (Governance Control Plane §16; ZS-IAM-001 §11). False is the
	// migration state: such commands are admitted, marked
	// X-Command-Contract: violated and logged, and an open-ended delegation
	// is bounded to the default term. Enforce is the default since 12 Oct
	// 2026, when the console and access-control-svc both send the fields;
	// AUTHZ_COMMAND_CONTRACT=warn is the explicit, logged way back.
	CommandContractEnforce bool

	// EvaluationMTLS is "require" or "permit" (internal/workload.Mode): whether
	// an evaluation call (/v1/authorize, /internal/*, the SoD / entity-scope /
	// delegated-access evaluators) must arrive with a verified client
	// certificate. Defaults to require everywhere except ENV=local, so a
	// deployment has to opt OUT of the check rather than into it
	// (Governance audit register A-02). AUTHZ_EVALUATION_MTLS overrides.
	EvaluationMTLS string

	// PEPAllowList names the services (client certificate CN, which
	// mtls-management-svc sets to service_name) allowed to call the
	// evaluation endpoints. Required in require mode; in permit mode an empty
	// list admits any certificate the platform CA issued.
	PEPAllowList []string

	// StepUp* are the server's step-up policy (Governance audit N-07; see
	// handler.StepUpPolicy). StepUpActions adds exact action names to the
	// built-in "reopen" set; StepUpMaxAuthnAgeSeconds is the oldest
	// authentication a step-up action accepts; StepUpUnknownAge is "step_up"
	// (default: an unstated age is not recent) or "permit" (the old reading,
	// for a deployment whose PEPs cannot forward an age yet — register A-03).
	StepUpActions            []string
	StepUpMaxAuthnAgeSeconds int
	StepUpUnknownAge         string

	// ReplicaID names this replica's cache-invalidation consumer group. Set it
	// to something stable across restarts; empty falls back to the hostname.
	ReplicaID string

	// IdentityAssertion is "require", "warn" or "off" (internal/identity.Mode):
	// whether a caller of the admin, session and console routes must prove who
	// it is with the identity envelope (or a registered delegate certificate)
	// instead of X-Principal-Id (Governance audit register A-01). Defaults to
	// require everywhere except ENV=local; "off" is refused outside local, and
	// "warn" outside local is an explicit, logged rollback.
	IdentityAssertion string
	// IdentityJWKSURL is identity-context-svc's /.well-known/jwks.json.
	IdentityJWKSURL string
	// IdentityIssuer / IdentityAudience are the envelope's iss and aud.
	IdentityIssuer   string
	IdentityAudience string
	// IdentityRevocation is "on" (default) or "off": whether a verified
	// envelope's session is checked at identity-context-svc (AU-01). off is
	// only allowed with ENV=local. IdentityIntrospectURL is the session status
	// endpoint; IdentityRevocationTTL caps how long a live answer is reused
	// (the revocation lag at this relying party); IdentityRevocationFail is
	// "closed" (default: 503 when the status is unknown) or "open".
	IdentityRevocation     string
	IdentityIntrospectURL  string
	IdentityRevocationTTL  time.Duration
	IdentityRevocationFail string
	// AdminDelegates are services (client certificate CNs) that may act on
	// the admin routes for a principal they forward, without that principal's
	// envelope: access-control-svc provisioning an approved grant.
	AdminDelegates []string

	// SessionStepUp is "enforce" (default) or "off" (ENV=local only): whether
	// issuing or approving a JIT or break-glass session needs a recent MFA
	// authentication (ZS-IAM-001 §13/§14; register A-03 / S-3).
	SessionStepUp string
	// SessionStepUpMaxAgeSeconds is the oldest MFA authentication that
	// qualifies, as "bounded age" (§3).
	SessionStepUpMaxAgeSeconds int

	// MaxJITSeconds, MaxBreakGlassSeconds and MaxSupportSeconds are the session
	// maximums (register B-01 / S-4). A longer request is refused 400, never
	// shortened. "Numeric limits require Security approval" (ZS-IAM-001 §31):
	// the defaults (8 h, 2 h, 4 h) were chosen 14 Oct 2026 and await that
	// sign-off.
	MaxJITSeconds        int
	MaxBreakGlassSeconds int
	MaxSupportSeconds    int

	// BreakGlassApprovals is how many independent, stepped-up approvers a
	// break-glass request needs before it is active (AU-02; GOV-12; decision
	// D-3, 14 Oct 2026: default one, quorum by risk class left to GOV §23).
	BreakGlassApprovals int
}

// Validate refuses a configuration under which no PEP could ever be admitted,
// or any certificate the CA issued would be. Neither is visible from outside,
// so both stop startup instead.
func (c *Config) Validate() error {
	switch c.StepUpUnknownAge {
	case "", "step_up", "permit": // "" is Load's default, step_up
	default:
		return fmt.Errorf("AUTHZ_STEP_UP_UNKNOWN_AGE must be \"step_up\" or \"permit\", got %q", c.StepUpUnknownAge)
	}
	// Outside a local stack the tenant of an entity the projection has not
	// heard of must be checked with the registry (Governance audit N-12):
	// without AUTHZ_ENTITY_REGISTRY_URL a claimed tenant is taken on trust for
	// those entities and the 12 Oct cross-tenant fix silently stops applying.
	if c.Env != "" && !strings.EqualFold(c.Env, "local") && strings.TrimSpace(c.EntityRegistryURL) == "" {
		return fmt.Errorf("AUTHZ_ENTITY_REGISTRY_URL is required outside ENV=local: without it the tenant of an unprojected entity is not verified")
	}
	local := c.Env == "" || strings.EqualFold(c.Env, "local")
	switch c.IdentityAssertion {
	case "", "require", "warn":
	case "off":
		if !local {
			return fmt.Errorf("AUTHZ_IDENTITY_ASSERTION=off is only allowed with ENV=local: every admin route would trust X-Principal-Id from anything on the network")
		}
	default:
		return fmt.Errorf("AUTHZ_IDENTITY_ASSERTION must be \"require\", \"warn\" or \"off\", got %q", c.IdentityAssertion)
	}
	// "" and 0 below are a Config built without Load (tests); Load always sets
	// them, so the checks apply to every real configuration.
	if c.IdentityAssertion != "" && c.IdentityAssertion != "off" && strings.TrimSpace(c.IdentityJWKSURL) == "" {
		return fmt.Errorf("AUTHZ_IDENTITY_JWKS_URL is required unless AUTHZ_IDENTITY_ASSERTION=off: no envelope could be verified")
	}
	switch c.IdentityRevocation {
	case "", "on":
	case "off":
		if !local {
			return fmt.Errorf("AUTHZ_IDENTITY_REVOCATION=off is only allowed with ENV=local: a logged-out session would keep opening the admin routes until its envelope expired (AU-01)")
		}
	default:
		return fmt.Errorf("AUTHZ_IDENTITY_REVOCATION must be \"on\" or \"off\", got %q", c.IdentityRevocation)
	}
	if c.IdentityRevocationTTL > time.Minute {
		return fmt.Errorf("AUTHZ_IDENTITY_REVOCATION_TTL_SECONDS above 60 exceeds the revocation SLO")
	}
	switch c.IdentityRevocationFail {
	case "", "closed", "open":
	default:
		return fmt.Errorf("AUTHZ_IDENTITY_REVOCATION_FAIL must be \"closed\" or \"open\", got %q", c.IdentityRevocationFail)
	}
	switch c.SessionStepUp {
	case "", "enforce":
	case "off":
		if !local {
			return fmt.Errorf("AUTHZ_SESSION_STEP_UP=off is only allowed with ENV=local: ZS-IAM-001 forbids password-only privileged sessions")
		}
	default:
		return fmt.Errorf("AUTHZ_SESSION_STEP_UP must be \"enforce\" or \"off\", got %q", c.SessionStepUp)
	}
	if c.SessionStepUpMaxAgeSeconds != 0 && c.SessionStepUpMaxAgeSeconds < 60 {
		return fmt.Errorf("AUTHZ_SESSION_STEP_UP_MAX_AGE_SECONDS must be at least 60")
	}
	for name, v := range map[string]int{
		"AUTHZ_MAX_JIT_SECONDS":         c.MaxJITSeconds,
		"AUTHZ_MAX_BREAK_GLASS_SECONDS": c.MaxBreakGlassSeconds,
		"AUTHZ_MAX_SUPPORT_SECONDS":     c.MaxSupportSeconds,
	} {
		if v != 0 && (v < 60 || v > 86400) {
			return fmt.Errorf("%s must be between 60 and 86400 seconds, got %d", name, v)
		}
	}
	// 0 is the default (one approver); break-glass is never self-activating.
	if c.BreakGlassApprovals < 0 || c.BreakGlassApprovals > 5 {
		return fmt.Errorf("AUTHZ_BREAK_GLASS_APPROVALS must be between 1 and 5, got %d", c.BreakGlassApprovals)
	}
	switch c.MTLSRevocationCheck {
	case "", "require": // "" is Load's default, require
	case "off":
		if c.MTLSEnabled && !strings.EqualFold(c.Env, "local") {
			return fmt.Errorf("MTLS_REVOCATION_CHECK=off is only allowed with ENV=local: a revoked client certificate would be admitted until it expires")
		}
	default:
		return fmt.Errorf("MTLS_REVOCATION_CHECK must be \"require\" or \"off\", got %q", c.MTLSRevocationCheck)
	}
	switch c.EvaluationMTLS {
	case "permit":
		return nil
	case "require":
	default:
		return fmt.Errorf("AUTHZ_EVALUATION_MTLS must be \"require\" or \"permit\", got %q", c.EvaluationMTLS)
	}
	if !c.MTLSEnabled {
		return fmt.Errorf("AUTHZ_EVALUATION_MTLS=require needs MTLS_ENABLED=true: no listener could carry a client certificate")
	}
	if len(c.PEPAllowList) == 0 {
		return fmt.Errorf("AUTHZ_EVALUATION_MTLS=require needs AUTHZ_PEP_ALLOWLIST: without it any certificate the platform CA issued is admitted")
	}
	return nil
}

// defaultEvaluationMTLS is require outside a local stack.
func defaultEvaluationMTLS(envName string) string {
	if strings.EqualFold(envName, "local") {
		return "permit"
	}
	return "require"
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
	// Empty means the server default, which is what a database-per-service
	// deployment uses: the service owns a whole database and its tables sit in
	// that database's public schema.
	//
	// A managed single-database host cannot express the 63 databases
	// deployments/init-db.sh creates, so each service gets a schema instead. The
	// migrations need no change -- they say CREATE TABLE, which lands wherever
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
// The password used to be interpolated bare, which silently produces a
// MALFORMED DSN for any password containing a space, a single quote or a
// backslash -- and managed database providers generate exactly those. The
// resulting connection error names whichever parameter the stray character
// happened to split, so it reads as a typo in an unrelated field rather than
// as a quoting problem.
//
// libpq's rule for a quoted value is that a backslash and a single quote are
// each escaped with a backslash. Written as an explicit loop rather than a
// strings.NewReplacer because the replacer's arguments would be raw string
// literals whose contents are themselves backslashes, where correct-by-one-level
// and wrong-by-two look identical at a glance.
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

	// DelegationTopic is delegated-authority-svc's event topic, CONSUMED (not
	// produced) so this service can project authority.delegated /
	// authority.revoked / authority.expired into its own
	// delegated_authorities table — see internal/events.Consumer for why
	// exactly these three and not the other events v1 declined to consume.
	//
	// Empty disables consumption entirely, leaving the delegation table fed
	// only by this service's own admin API, which is where it was before.
	// That is the off switch for a deployment where delegated-authority-svc
	// is not running: the consumer would otherwise sit retrying a topic that
	// never appears.
	DelegationTopic string

	// DelegationGroupID is the consumer group for the above. Distinct from
	// GroupID, which names this service as a PRODUCER — sharing one id
	// between a producer and a consumer reads as a single logical
	// subscription and makes offsets impossible to reason about.
	DelegationGroupID string

	// LifecycleTopics are the topics carrying the other three consumed-event
	// concepts Doc 03 §8.3 names — see internal/events.LifecycleConsumer for
	// the mapping from spec name to real event, which is not one-to-one:
	//
	//	zoiko.identity.events        principal.status.changed
	//	                             (§8.3's employment.changed) — PROJECTED
	//	                             into principal_status_projection and read
	//	                             as layer 0 of /v1/authorize.
	//	zoiko.access-control.events  role.created / role.updated /
	//	                             permission.bundle.updated
	//	                             (§8.3's role.assigned)
	//	zoiko.entity.events          entity.status.changed /
	//	                             entity.hierarchy.changed /
	//	                             entity.jurisdiction.changed
	//	                             (§8.3's entity.scope.updated)
	//
	// ONE reader over three topics, via kafka.ReaderConfig.GroupTopics,
	// rather than three readers: they share a dispatch table, a dedupe set
	// and a consumer group, and three readers would mean three offset streams
	// to reason about for one logical subscription.
	//
	// Comma-separated, and EMPTY DISABLES the consumer entirely — the off
	// switch for a deployment where these services are not running, which
	// would otherwise leave a reader retrying topics that never appear. With
	// it off, layer 0 stays inert (no row means ACTIVE) and the cache TTL is
	// once again the only bound on cross-replica staleness, which is where
	// this service was before the consumer existed.
	LifecycleTopics []string

	// LifecycleGroupID is the consumer group for the above. Distinct from
	// both GroupID and DelegationGroupID for the same reason those are
	// distinct from each other.
	LifecycleGroupID string
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	envName := env("ENV", "local")
	return &Config{
		Env: envName,
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
			// Empty keeps the server default schema, which is what the
			// database-per-service local stack uses. Set to this service's
			// schema name when the database is a managed single-database host.
			Schema: env("DB_SCHEMA", ""),
			// pgx tuning that varies by deployment, not by service. Required
			// when connecting through a transaction-mode connection pooler:
			// "default_query_exec_mode=exec statement_cache_capacity=0".
			Options: env("DB_OPTIONS", ""),
		},
		Kafka: KafkaConfig{
			Brokers: strings.Split(env("KAFKA_BROKERS", "localhost:9092"), ","),
			GroupID: env("KAFKA_GROUP_ID", "authorization-svc"),
			Topic:   env("KAFKA_EVENTS_TOPIC", "zoiko.authorization.events"),
			// Defaulted rather than opt-in: the topic name is
			// delegated-authority-svc's own compose default, so a stack
			// running both services projects delegations with no extra
			// configuration. Set KAFKA_DELEGATION_TOPIC="" to disable.
			// envAllowEmpty, not env: env() folds a set-but-empty variable
			// into its default, so with it there would be no way to express
			// "off" at all.
			DelegationTopic:   envAllowEmpty("KAFKA_DELEGATION_TOPIC", "zoiko.delegated-authority.events"),
			DelegationGroupID: env("KAFKA_DELEGATION_GROUP_ID", "authorization-svc-delegation-projector"),

			// Defaulted to the three producers' own compose defaults, on the
			// same reasoning as DelegationTopic: a stack running those
			// services is consumed from with no extra configuration.
			// envAllowEmpty so KAFKA_LIFECYCLE_TOPICS="" is a real off switch
			// — env() would fold the empty value back into this default and
			// there would be no way to express "off".
			LifecycleTopics: splitTopics(envAllowEmpty("KAFKA_LIFECYCLE_TOPICS",
				"zoiko.identity.events,zoiko.access-control.events,zoiko.entity.events")),
			LifecycleGroupID: env("KAFKA_LIFECYCLE_GROUP_ID", "authorization-svc-lifecycle"),
		},
		JurisdictionRulesURL: env("JURISDICTION_RULES_URL", "http://jurisdiction-rules-svc:8082"),
		OTELExporterEndpoint: env("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318"),

		MTLSEnabled:              envBool("MTLS_ENABLED", false),
		MTLSPort:                 envInt("MTLS_PORT", 8449),
		MTLSManagementServiceURL: env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"),
		MTLSBootstrapTokenPath:   env("MTLS_BOOTSTRAP_TOKEN_PATH", ""),
		MTLSRevocationCheck:      strings.ToLower(env("MTLS_REVOCATION_CHECK", "require")),
		MTLSCRLURL:               env("MTLS_CRL_URL", strings.TrimRight(env("MTLS_MANAGEMENT_SERVICE_URL", "http://mtls-management-svc:8140"), "/")+"/v1/mtls/crl"),
		MTLSCRLRefreshSeconds:    envInt("MTLS_CRL_REFRESH_SECONDS", 30),
		SIEMServiceURL:           env("SIEM_SERVICE_URL", ""),
		PlatformScopeEntityID:    env("AUTHZ_PLATFORM_SCOPE_ENTITY_ID", ""),

		// Seconds, not a duration string: every other numeric knob in this
		// service is an int env var, and "5s" vs "5" is the kind of
		// difference that turns a cache into a no-op without an error.
		CacheTTL: time.Duration(envInt("AUTHZ_CACHE_TTL_SECONDS", 5)) * time.Second,

		// 24 months by default, which is a retention WINDOW rather than a
		// deletion: the function it configures detaches partitions, and an
		// operator archives them. 0 disables detaching entirely.
		AccessDecisionRetentionMonths: envInt("AUTHZ_ACCESS_DECISION_RETENTION_MONTHS", 24),

		// How often the retention sweep runs. Daily is ample: both halves of the
		// sweep are month-granular, so the only thing a shorter interval buys is
		// a faster recovery after a failed sweep. 0 disables the sweeper entirely,
		// which leaves the partition runway to be extended by hand — the
		// behaviour before the sweeper existed.
		RetentionSweepInterval: time.Duration(envInt("AUTHZ_RETENTION_SWEEP_INTERVAL_HOURS", 24)) * time.Hour,

		// How many months of partitions to keep ahead of the current one. Three
		// is a runway, not a guess: it means the sweep can fail silently for two
		// consecutive months before any decision lands in the default partition.
		PartitionMonthsAhead: envInt("AUTHZ_PARTITION_MONTHS_AHEAD", 3),

		// Seven days each: a published event is in Kafka, and an admin
		// command retried a week later is a new command.
		OutboxRetention:      time.Duration(envInt("AUTHZ_OUTBOX_RETENTION_HOURS", 168)) * time.Hour,
		IdempotencyRetention: time.Duration(envInt("AUTHZ_IDEMPOTENCY_RETENTION_HOURS", 168)) * time.Hour,

		// EnforceTenantOnAuthorize controls whether /v1/authorize requires a tenant
		// scope (via X-Tenant-Id header or tenant_id in body). When false (default),
		// tenantless requests are allowed but logged with a warning — they are
		// evaluated against globally-applicable SoD rules only. When true, a missing
		// tenant scope returns 400. This is a migration aid for the ~86 callers that
		// do not yet forward X-Tenant-Id; once they are all migrated, this should be
		// set to true.
		EnforceTenantOnAuthorize: envBool("AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE", false),
		CommandContractEnforce:   !strings.EqualFold(env("AUTHZ_COMMAND_CONTRACT", "enforce"), "warn"),
		EntityRegistryURL:        env("AUTHZ_ENTITY_REGISTRY_URL", ""),
		EvaluationMTLS:           strings.ToLower(env("AUTHZ_EVALUATION_MTLS", defaultEvaluationMTLS(envName))),
		PEPAllowList:             splitTopics(env("AUTHZ_PEP_ALLOWLIST", "")),
		StepUpActions:            splitTopics(env("AUTHZ_STEP_UP_ACTIONS", "")),
		StepUpMaxAuthnAgeSeconds: envInt("AUTHZ_STEP_UP_MAX_AUTHN_AGE_SECONDS", 300),
		StepUpUnknownAge:         strings.ToLower(env("AUTHZ_STEP_UP_UNKNOWN_AGE", "step_up")),
		ReplicaID:                strings.TrimSpace(env("AUTHZ_REPLICA_ID", "")),

		IdentityAssertion: strings.ToLower(env("AUTHZ_IDENTITY_ASSERTION", defaultIdentityAssertion(envName))),
		IdentityJWKSURL:   env("AUTHZ_IDENTITY_JWKS_URL", ""),
		IdentityIssuer:    env("AUTHZ_IDENTITY_ISSUER", "identity-context-svc"),
		IdentityAudience:  env("AUTHZ_IDENTITY_AUDIENCE", "zoiko-internal"),
		AdminDelegates:    splitTopics(env("AUTHZ_ADMIN_DELEGATES", "")),

		IdentityRevocation:     strings.ToLower(env("AUTHZ_IDENTITY_REVOCATION", "on")),
		IdentityIntrospectURL:  env("AUTHZ_IDENTITY_INTROSPECT_URL", "http://identity-context-svc:8080/v1/context/session/introspect"),
		IdentityRevocationTTL:  time.Duration(envInt("AUTHZ_IDENTITY_REVOCATION_TTL_SECONDS", 5)) * time.Second,
		IdentityRevocationFail: strings.ToLower(env("AUTHZ_IDENTITY_REVOCATION_FAIL", "closed")),

		SessionStepUp:              strings.ToLower(env("AUTHZ_SESSION_STEP_UP", "enforce")),
		SessionStepUpMaxAgeSeconds: envInt("AUTHZ_SESSION_STEP_UP_MAX_AGE_SECONDS", 900),

		MaxJITSeconds:        envInt("AUTHZ_MAX_JIT_SECONDS", 8*3600),
		MaxBreakGlassSeconds: envInt("AUTHZ_MAX_BREAK_GLASS_SECONDS", 2*3600),
		MaxSupportSeconds:    envInt("AUTHZ_MAX_SUPPORT_SECONDS", 4*3600),

		BreakGlassApprovals: envInt("AUTHZ_BREAK_GLASS_APPROVALS", 1),
	}, nil
}

// defaultIdentityAssertion is require outside a local stack.
func defaultIdentityAssertion(envName string) string {
	if strings.EqualFold(envName, "local") {
		return "warn"
	}
	return "require"
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envAllowEmpty is env for a setting whose EMPTY value is meaningful.
//
// env() cannot express that: it treats a set-but-empty variable as unset and
// substitutes the default, so a feature defaulted to on can never be turned
// off by clearing its variable — the clearing silently re-enables it. This
// distinguishes "unset" from "set to empty" with LookupEnv.
func envAllowEmpty(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// splitTopics parses a comma-separated topic list, dropping empties.
//
// Returns nil for an empty or all-whitespace value rather than a one-element
// slice containing "", which is what strings.Split does and which would have
// the consumer subscribe to a topic named empty string — a subscription that
// never delivers and never errors, i.e. the off switch appearing to work while
// leaving a reader spinning. Whitespace around each name is trimmed because
// "a, b" in an env var or a compose file is what a person writes.
func splitTopics(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
