// Package config loads the public-ai-svc runtime configuration from
// environment variables (IMPLEMENTATION_PROMPT §7, Doc 04 App. B).
//
// Every numeric limit here is a DEV DEFAULT: Docs 05/06/08 deliberately leave
// TTLs, rate limits and spend ceilings to be ratified (DEC-07, DEC-08, D06-05).
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Enabled  bool
	HTTPAddr string
	// AdminAddr, when set, is a separate internal listener that serves
	// /metrics and /health/* (Doc 06 §12); the public listener then serves
	// only /public-ai/v1. Unset keeps both on HTTPAddr (dev, compose probes).
	AdminAddr    string
	DatabaseURL  string
	KafkaBrokers []string

	AllowedOrigins []string
	// AllowedHosts, when set, restricts the Host header (Doc 04 §08, A02).
	AllowedHosts []string
	// ModelProfile is derived: ModelProfile(ModelProvider, ModelID).
	ModelProfile  string
	PolicyVersion string
	Region        string

	TokenBudgetPerTurn       int
	MaxActiveTurnsPerSession int
	SessionTTL               time.Duration
	// PolicyDigest is the approved policy fingerprint (required outside
	// dev/test, Doc 06 §08). ProviderApprovalRef and ProviderRegion record
	// the DPA/transfer approval for a remote model provider (SEC-P0-06).
	PolicyDigest        string
	ProviderApprovalRef string
	ProviderRegion      string
	ApprovedRegions     []string // DPA/transfer-approved regions (DEC-05)
	// SessionIdleTTL ends a session with no completed turn before SessionTTL
	// (Doc 06 §17); SessionTokenCap is the per-session token cap (§13).
	SessionIdleTTL  time.Duration
	SessionTokenCap int64
	IdempotencyTTL  time.Duration
	// Retention is how long expired/deleted sessions and their turns are
	// kept before purge. Dev default pending DPO approval (DEC-06).
	Retention time.Duration
	// Environment is dev, test, staging or production. Outside dev/test the
	// retention period must come from the signed policy config, never from a
	// default (Master prompt §10): RetentionSet records whether it was given.
	Environment  string
	RetentionSet bool

	// ModelProvider selects the model gateway backend. Only "mock" (the
	// deterministic extractive provider, tools always empty) exists in this
	// build, and only for dev/test (DEC-05). ModelID must be allowlisted.
	ModelProvider   string
	ModelID         string
	AllowedModelIDs []string
	// GroqAPIKey (GROQ_API_KEY) is the credential for the approved model
	// provider; it is server-only and never logged. The model comes from
	// GROQ_MODEL (default openai/gpt-oss-120b) and must be allowlisted.
	GroqAPIKey string
	// Dataset is what local dev mode serves: "seed" (data/seed, synthetic)
	// or "fixtures" (testdata/fixtures). SeedOnStart publishes and activates
	// that dataset into the database on start (dev/test only).
	Dataset     string
	SeedOnStart bool
	// KBAllowDraft serves DRAFT_PENDING_REVIEW articles (labelled "Draft
	// content") in local/dev only; it is refused anywhere else.
	KBAllowDraft bool
	// StructuredResponseDev turns structured_response_enabled on at start
	// for the in-memory or seeded dev dataset (dev/test only). Elsewhere the
	// flag changes only through an audited flags.Set.
	StructuredResponseDev     bool
	MaxBodyBytes              int64
	ModelTimeout              time.Duration
	RateLimitPerIPPerMinute   int
	RateLimitPerSessionPerMin int
	// RateLimitSessionCreatePerMin is the tighter per-IP limit on new
	// sessions (Doc 06 §13, SEC-T055).
	RateLimitSessionCreatePerMin int
	// TrustedProxies are the CIDRs whose X-Forwarded-For is believed when
	// keying the per-IP limit (the console's same-origin proxy, the edge).
	TrustedProxies      []string
	DailyTokenCeiling   int64
	MonthlyTokenCeiling int64

	// Hard privacy switches. LogRawPrompt and ProductionChatPIICapture are
	// read only so Load can refuse to start when someone turns them on: R0
	// never logs or stores raw visitor text (Doc 06 §21, SEC-P0-07).
	LogRawPrompt             bool
	ProductionChatPIICapture bool

	// Ed25519 keys, base64 (std encoding). The public key verifies signed
	// knowledge/claim releases; the private key is only needed by kbctl.
	SigningPublicKey string
	SigningKey       string
}

// Load reads the environment and validates it.
func Load() (Config, error) {
	c := Config{
		Enabled: envBool("PUBLIC_AI_ENABLED", false),
		// Dev listens where the console expects it (ZOIKO_PUBLIC_AI_URL default)
		// and off :8080, which local Docker stacks commonly hold. Containers set
		// PUBLIC_AI_HTTP_ADDR explicitly.
		HTTPAddr:                 env("PUBLIC_AI_HTTP_ADDR", devDefault(os.Getenv("PUBLIC_AI_ENVIRONMENT"), "127.0.0.1:8001", ":8080")),
		AdminAddr:                os.Getenv("PUBLIC_AI_ADMIN_ADDR"),
		DatabaseURL:              os.Getenv("PUBLIC_AI_DATABASE_URL"),
		KafkaBrokers:             splitList(os.Getenv("PUBLIC_AI_KAFKA_BROKERS")),
		AllowedOrigins:           splitList(env("PUBLIC_AI_ALLOWED_ORIGINS", "http://localhost:3000")),
		AllowedHosts:             splitList(os.Getenv("PUBLIC_AI_ALLOWED_HOSTS")),
		PolicyVersion:            env("PUBLIC_AI_POLICY_VERSION", "pub-policy-1.0"),
		Region:                   os.Getenv("PUBLIC_AI_REGION"),
		TokenBudgetPerTurn:       envInt("PUBLIC_AI_TOKEN_BUDGET_PER_TURN", 2000),
		MaxActiveTurnsPerSession: envInt("PUBLIC_AI_MAX_ACTIVE_TURNS_PER_SESSION", 1),
		SessionTTL:               envDuration("PUBLIC_AI_SESSION_TTL", 30*time.Minute),
		SessionIdleTTL:           envDuration("PUBLIC_AI_SESSION_IDLE_TTL", 15*time.Minute),
		// A grounded turn costs about 3k tokens (system prompt + evidence),
		// so the dev default allows about 30 answers per conversation. The
		// approved value is DEC-07/08; outside dev/test it must be set.
		SessionTokenCap: int64(envInt("PUBLIC_AI_SESSION_TOKEN_CAP", 100000)),
		IdempotencyTTL:  envDuration("PUBLIC_AI_IDEMPOTENCY_TTL", 24*time.Hour),
		Retention:       envDuration("PUBLIC_AI_RETENTION", 24*time.Hour),
		// Required and never assumed (SEC-P0-06): an unset value used to
		// mean dev and switch the production gates off.
		Environment:                  os.Getenv("PUBLIC_AI_ENVIRONMENT"),
		PolicyDigest:                 os.Getenv("PUBLIC_AI_POLICY_DIGEST"),
		ProviderApprovalRef:          os.Getenv("PUBLIC_AI_PROVIDER_APPROVAL_REF"),
		ProviderRegion:               os.Getenv("PUBLIC_AI_REGION"),
		ApprovedRegions:              splitList(os.Getenv("PUBLIC_AI_APPROVED_REGIONS")),
		RetentionSet:                 os.Getenv("PUBLIC_AI_RETENTION") != "",
		ModelProvider:                env("PUBLIC_AI_MODEL_PROVIDER", env("MODEL_PROVIDER", "mock")),
		ModelID:                      modelIDFor(env("PUBLIC_AI_MODEL_PROVIDER", env("MODEL_PROVIDER", "mock"))),
		AllowedModelIDs:              splitList(env("PUBLIC_AI_ALLOWED_MODEL_IDS", "mock-extractive-v1,openai/gpt-oss-120b")),
		GroqAPIKey:                   os.Getenv("GROQ_API_KEY"),
		Dataset:                      env("PUBLIC_AI_DATASET", "public-kb"),
		KBAllowDraft:                 envBool("KB_ALLOW_DRAFT", envDefaultDev()),
		SeedOnStart:                  envBool("PUBLIC_AI_SEED_ON_START", false),
		StructuredResponseDev:        envBool("PUBLIC_AI_STRUCTURED_RESPONSE", envDefaultDev()),
		MaxBodyBytes:                 int64(envInt("PUBLIC_AI_MAX_BODY_BYTES", 16*1024)),
		ModelTimeout:                 envDuration("PUBLIC_AI_MODEL_TIMEOUT", 6*time.Second),
		RateLimitPerIPPerMinute:      envInt("PUBLIC_AI_RATE_LIMIT_IP_PER_MIN", 60),
		RateLimitPerSessionPerMin:    envInt("PUBLIC_AI_RATE_LIMIT_SESSION_PER_MIN", 20),
		RateLimitSessionCreatePerMin: envInt("PUBLIC_AI_RATE_LIMIT_SESSION_CREATE_PER_MIN", 10),
		TrustedProxies:               splitList(env("PUBLIC_AI_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")),
		DailyTokenCeiling:            int64(envInt("PUBLIC_AI_DAILY_TOKEN_CEILING", 2_000_000)),
		MonthlyTokenCeiling:          int64(envInt("PUBLIC_AI_MONTHLY_TOKEN_CEILING", 40_000_000)),
		LogRawPrompt:                 envBool("PUBLIC_AI_LOG_RAW_PROMPT", false),
		ProductionChatPIICapture:     envBool("PUBLIC_AI_PRODUCTION_CHAT_PII_CAPTURE", false),
		SigningPublicKey:             os.Getenv("PUBLIC_AI_SIGNING_PUBLIC_KEY"),
		SigningKey:                   os.Getenv("PUBLIC_AI_SIGNING_KEY"),
	}
	// The model profile is derived, never configured on its own, so the
	// manifest, eval report and build_info name the model that actually runs.
	c.ModelProfile = ModelProfile(c.ModelProvider, c.ModelID)
	return c, c.Validate()
}

// ModelProfile is the release-tuple model identity "<provider>:<model id>"
// (Doc 08 §06), e.g. "groq:openai/gpt-oss-120b" or "mock:mock-extractive-v1".
func ModelProfile(provider, modelID string) string { return provider + ":" + modelID }

// ModelProfileFromEnv derives the profile the server would run with from
// the same variables Load reads (for offline tools such as kbctl).
func ModelProfileFromEnv() string {
	p := env("PUBLIC_AI_MODEL_PROVIDER", env("MODEL_PROVIDER", "mock"))
	return ModelProfile(p, modelIDFor(p))
}

// MockModelID is the deterministic mock provider's model id.
const MockModelID = "mock-extractive-v1"

// Validate rejects configurations that would breach a non-waivable control.
func (c Config) Validate() error {
	if c.LogRawPrompt {
		return fmt.Errorf("PUBLIC_AI_LOG_RAW_PROMPT=true is not permitted in R0 (Doc 06 §21)")
	}
	if c.ProductionChatPIICapture {
		return fmt.Errorf("PUBLIC_AI_PRODUCTION_CHAT_PII_CAPTURE=true is not permitted in R0 (INV-06)")
	}
	if c.MaxActiveTurnsPerSession != 1 {
		return fmt.Errorf("PUBLIC_AI_MAX_ACTIVE_TURNS_PER_SESSION must be 1 (Doc 05 §12)")
	}
	for _, o := range c.AllowedOrigins {
		if o == "*" {
			return fmt.Errorf("wildcard origin is not permitted (Doc 06 §11)")
		}
	}
	for _, p := range c.TrustedProxies {
		_, n, err := net.ParseCIDR(p)
		if err != nil {
			return fmt.Errorf("PUBLIC_AI_TRUSTED_PROXIES: %q is not a CIDR", p)
		}
		if ones, _ := n.Mask.Size(); ones == 0 {
			return fmt.Errorf("PUBLIC_AI_TRUSTED_PROXIES must not trust every address")
		}
	}
	devLike := c.Environment == "dev" || c.Environment == "test"
	if !knownEnvironments[c.Environment] {
		return fmt.Errorf("PUBLIC_AI_ENVIRONMENT is required: dev, test, staging or production (got %q)", c.Environment)
	}
	// Reject an unknown provider before the provider approval gates: otherwise a
	// typo reads as a missing DPA or region instead of the real mistake.
	if c.ModelProvider != "mock" && c.ModelProvider != "groq" {
		return fmt.Errorf("model provider %q is not available in this build (mock or groq)", c.ModelProvider)
	}
	switch {
	case c.ModelProvider == "mock" && !devLike:
		return fmt.Errorf("the mock model provider is dev/test only (PUBLIC_AI_ENVIRONMENT=%s)", c.Environment)
	case c.ModelProvider == "groq" && c.GroqAPIKey == "":
		return fmt.Errorf("MODEL_PROVIDER=groq needs GROQ_API_KEY in the server environment")
	case c.ModelProvider != "mock" && !devLike && c.ProviderApprovalRef == "":
		return fmt.Errorf("a remote model provider outside dev/test needs PUBLIC_AI_PROVIDER_APPROVAL_REF (DPA/transfer approval, SEC-P0-06)")
	case c.ModelProvider != "mock" && !devLike && (c.ProviderRegion == "" || !contains(c.ApprovedRegions, c.ProviderRegion)):
		return fmt.Errorf("PUBLIC_AI_REGION %q is not in PUBLIC_AI_APPROVED_REGIONS: no provider request without an approved region (SEC-P0-06)", c.ProviderRegion)
	case !devLike && c.PolicyDigest == "":
		return fmt.Errorf("PUBLIC_AI_POLICY_DIGEST is required outside dev/test: the service runs only the approved policy (Doc 06 A08)")
	case !devLike && len(c.AllowedHosts) == 0:
		return fmt.Errorf("PUBLIC_AI_ALLOWED_HOSTS is required outside dev/test: requests for any other Host are refused (Doc 06 A11)")
	case !contains(c.AllowedModelIDs, c.ModelID):
		return fmt.Errorf("model id %q is not allowlisted (PUBLIC_AI_ALLOWED_MODEL_IDS)", c.ModelID)
	case c.Dataset != "public-kb" && c.Dataset != "seed" && c.Dataset != "fixtures":
		return fmt.Errorf("PUBLIC_AI_DATASET must be public-kb, seed or fixtures")
	case c.KBAllowDraft && !devLike:
		return fmt.Errorf("KB_ALLOW_DRAFT is local/dev only: production content must pass two-person approval")
	case c.SeedOnStart && !devLike:
		return fmt.Errorf("PUBLIC_AI_SEED_ON_START is dev/test only")
	case c.StructuredResponseDev && !devLike:
		return fmt.Errorf("PUBLIC_AI_STRUCTURED_RESPONSE is dev/test only: elsewhere set structured_response_enabled with an approval")
	case !devLike && c.DatabaseURL == "":
		return fmt.Errorf("PUBLIC_AI_DATABASE_URL is required outside dev/test: refusing to fall back to in-memory storage")
	}
	if err := validateRetention(c); err != nil {
		return err
	}
	if c.SessionTTL <= 0 || c.IdempotencyTTL <= 0 || c.ModelTimeout <= 0 {
		return fmt.Errorf("TTL and timeout values must be positive")
	}
	if c.ModelTimeout > MaxModelTimeout {
		return fmt.Errorf("PUBLIC_AI_MODEL_TIMEOUT %s exceeds %s: drain (%s) + 2 x timeout + %s must fit the %s stop grace period (Doc 08 §11, OPR-012)",
			c.ModelTimeout, MaxModelTimeout, DrainGrace, ShutdownSlack, StopGracePeriod)
	}
	if c.AdminAddr != "" && c.AdminAddr == c.HTTPAddr {
		return fmt.Errorf("PUBLIC_AI_ADMIN_ADDR must differ from PUBLIC_AI_HTTP_ADDR")
	}
	if !devLike {
		if err := validateProductionLimits(c); err != nil {
			return err
		}
	}
	return nil
}

// Shutdown budget (Doc 08 §11, OPR-012): on SIGTERM the server reports
// not-ready for DrainGrace, then waits 2 x ModelTimeout + ShutdownSlack for
// in-flight turns. That must fit the orchestrator's stop grace period
// (stop_grace_period: 45s in docker-compose.public-ai.yml; set
// terminationGracePeriodSeconds to at least the same elsewhere), so the
// model timeout is bounded by MaxModelTimeout = (45 - 5 - 15) / 2 = 12.5 s.
const (
	DrainGrace      = 5 * time.Second
	ShutdownSlack   = 15 * time.Second
	StopGracePeriod = 45 * time.Second
	MaxModelTimeout = (StopGracePeriod - DrainGrace - ShutdownSlack) / 2
)

// validateProductionLimits: outside dev/test every abuse and cost limit is
// set (zero or negative would mean "no limit") and every allowed origin is a
// real https origin, never localhost (SEC-P0-09, D06-09).
func validateProductionLimits(c Config) error {
	for name, v := range map[string]int64{
		"PUBLIC_AI_RATE_LIMIT_IP_PER_MIN":             int64(c.RateLimitPerIPPerMinute),
		"PUBLIC_AI_RATE_LIMIT_SESSION_PER_MIN":        int64(c.RateLimitPerSessionPerMin),
		"PUBLIC_AI_RATE_LIMIT_SESSION_CREATE_PER_MIN": int64(c.RateLimitSessionCreatePerMin),
		"PUBLIC_AI_DAILY_TOKEN_CEILING":               c.DailyTokenCeiling,
		"PUBLIC_AI_MONTHLY_TOKEN_CEILING":             c.MonthlyTokenCeiling,
		"PUBLIC_AI_SESSION_TOKEN_CAP":                 c.SessionTokenCap,
		"PUBLIC_AI_TOKEN_BUDGET_PER_TURN":             int64(c.TokenBudgetPerTurn),
	} {
		if v <= 0 {
			return fmt.Errorf("%s must be > 0 outside dev/test (SEC-P0-09)", name)
		}
	}
	if len(c.AllowedOrigins) == 0 {
		return fmt.Errorf("PUBLIC_AI_ALLOWED_ORIGINS is required outside dev/test")
	}
	for _, o := range c.AllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" {
			return fmt.Errorf("PUBLIC_AI_ALLOWED_ORIGINS: %q must be an https origin outside dev/test (SEC-P0-09)", o)
		}
		h := strings.ToLower(u.Hostname())
		if h == "localhost" || strings.HasSuffix(h, ".localhost") {
			return fmt.Errorf("PUBLIC_AI_ALLOWED_ORIGINS: %q is localhost, not permitted outside dev/test", o)
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return fmt.Errorf("PUBLIC_AI_ALLOWED_ORIGINS: %q is a loopback address, not permitted outside dev/test", o)
		}
	}
	return nil
}

var knownEnvironments = map[string]bool{"dev": true, "test": true, "staging": true, "production": true}

// validateRetention: outside dev/test the retention period must come from the
// signed policy, never from a default (Master prompt §10).
func validateRetention(c Config) error {
	if c.Environment != "dev" && c.Environment != "test" && !c.RetentionSet {
		return fmt.Errorf("PUBLIC_AI_RETENTION must be set from the signed retention policy outside dev/test (TODO(DPO): DEC-06)")
	}
	return nil
}

// envDefaultDev is true when PUBLIC_AI_ENVIRONMENT is dev or test; unset is
// not dev, so dev-only defaults stay off.
func envDefaultDev() bool {
	e := os.Getenv("PUBLIC_AI_ENVIRONMENT")
	return e == "dev" || e == "test"
}

// modelIDFor is the configured model for a provider: PUBLIC_AI_MODEL_ID,
// else GROQ_MODEL for groq, else the provider's default.
func modelIDFor(provider string) string {
	if v := os.Getenv("PUBLIC_AI_MODEL_ID"); v != "" {
		return v
	}
	if provider == "groq" {
		return env("GROQ_MODEL", "openai/gpt-oss-120b")
	}
	return MockModelID
}

func devDefault(environment, dev, other string) string {
	if environment == "dev" || environment == "test" {
		return dev
	}
	return other
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func splitList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
