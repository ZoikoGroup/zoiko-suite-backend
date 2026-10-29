// Package config loads the public-ai-svc runtime configuration from
// environment variables (IMPLEMENTATION_PROMPT §7, Doc 04 App. B).
//
// Every numeric limit here is a DEV DEFAULT: Docs 05/06/08 deliberately leave
// TTLs, rate limits and spend ceilings to be ratified (DEC-07, DEC-08, D06-05).
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Enabled      bool
	HTTPAddr     string
	DatabaseURL  string
	KafkaBrokers []string

	AllowedOrigins []string
	// AllowedHosts, when set, restricts the Host header (Doc 04 §08, A02).
	AllowedHosts  []string
	ModelProfile  string
	PolicyVersion string
	Region        string

	TokenBudgetPerTurn       int
	MaxActiveTurnsPerSession int
	SessionTTL               time.Duration
	IdempotencyTTL           time.Duration
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
	KBAllowDraft              bool
	MaxBodyBytes              int64
	ModelTimeout              time.Duration
	RateLimitPerIPPerMinute   int
	RateLimitPerSessionPerMin int
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
		HTTPAddr:                  env("PUBLIC_AI_HTTP_ADDR", devDefault(env("PUBLIC_AI_ENVIRONMENT", "dev"), "127.0.0.1:8001", ":8080")),
		DatabaseURL:               os.Getenv("PUBLIC_AI_DATABASE_URL"),
		KafkaBrokers:              splitList(os.Getenv("PUBLIC_AI_KAFKA_BROKERS")),
		AllowedOrigins:            splitList(env("PUBLIC_AI_ALLOWED_ORIGINS", "http://localhost:3000")),
		AllowedHosts:              splitList(os.Getenv("PUBLIC_AI_ALLOWED_HOSTS")),
		ModelProfile:              env("PUBLIC_AI_MODEL_PROFILE", "fake-v0"),
		PolicyVersion:             env("PUBLIC_AI_POLICY_VERSION", "pub-policy-1.0"),
		Region:                    os.Getenv("PUBLIC_AI_REGION"),
		TokenBudgetPerTurn:        envInt("PUBLIC_AI_TOKEN_BUDGET_PER_TURN", 2000),
		MaxActiveTurnsPerSession:  envInt("PUBLIC_AI_MAX_ACTIVE_TURNS_PER_SESSION", 1),
		SessionTTL:                envDuration("PUBLIC_AI_SESSION_TTL", 30*time.Minute),
		IdempotencyTTL:            envDuration("PUBLIC_AI_IDEMPOTENCY_TTL", 24*time.Hour),
		Retention:                 envDuration("PUBLIC_AI_RETENTION", 24*time.Hour),
		Environment:               env("PUBLIC_AI_ENVIRONMENT", "dev"),
		RetentionSet:              os.Getenv("PUBLIC_AI_RETENTION") != "",
		ModelProvider:             env("PUBLIC_AI_MODEL_PROVIDER", env("MODEL_PROVIDER", "mock")),
		ModelID:                   modelIDFor(env("PUBLIC_AI_MODEL_PROVIDER", env("MODEL_PROVIDER", "mock"))),
		AllowedModelIDs:           splitList(env("PUBLIC_AI_ALLOWED_MODEL_IDS", "mock-extractive-v1,openai/gpt-oss-120b")),
		GroqAPIKey:                os.Getenv("GROQ_API_KEY"),
		Dataset:                   env("PUBLIC_AI_DATASET", "public-kb"),
		KBAllowDraft:              envBool("KB_ALLOW_DRAFT", envDefaultDev()),
		SeedOnStart:               envBool("PUBLIC_AI_SEED_ON_START", false),
		MaxBodyBytes:              int64(envInt("PUBLIC_AI_MAX_BODY_BYTES", 16*1024)),
		ModelTimeout:              envDuration("PUBLIC_AI_MODEL_TIMEOUT", 6*time.Second),
		RateLimitPerIPPerMinute:   envInt("PUBLIC_AI_RATE_LIMIT_IP_PER_MIN", 60),
		RateLimitPerSessionPerMin: envInt("PUBLIC_AI_RATE_LIMIT_SESSION_PER_MIN", 20),
		TrustedProxies:            splitList(env("PUBLIC_AI_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")),
		DailyTokenCeiling:         int64(envInt("PUBLIC_AI_DAILY_TOKEN_CEILING", 2_000_000)),
		MonthlyTokenCeiling:       int64(envInt("PUBLIC_AI_MONTHLY_TOKEN_CEILING", 40_000_000)),
		LogRawPrompt:              envBool("PUBLIC_AI_LOG_RAW_PROMPT", false),
		ProductionChatPIICapture:  envBool("PUBLIC_AI_PRODUCTION_CHAT_PII_CAPTURE", false),
		SigningPublicKey:          os.Getenv("PUBLIC_AI_SIGNING_PUBLIC_KEY"),
		SigningKey:                os.Getenv("PUBLIC_AI_SIGNING_KEY"),
	}
	return c, c.Validate()
}

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
	switch {
	case c.ModelProvider != "mock" && c.ModelProvider != "groq":
		return fmt.Errorf("model provider %q is not available in this build (mock or groq)", c.ModelProvider)
	case c.ModelProvider == "mock" && !devLike:
		return fmt.Errorf("the mock model provider is dev/test only (PUBLIC_AI_ENVIRONMENT=%s)", c.Environment)
	case c.ModelProvider == "groq" && c.GroqAPIKey == "":
		return fmt.Errorf("MODEL_PROVIDER=groq needs GROQ_API_KEY in the server environment")
	case !contains(c.AllowedModelIDs, c.ModelID):
		return fmt.Errorf("model id %q is not allowlisted (PUBLIC_AI_ALLOWED_MODEL_IDS)", c.ModelID)
	case c.Dataset != "public-kb" && c.Dataset != "seed" && c.Dataset != "fixtures":
		return fmt.Errorf("PUBLIC_AI_DATASET must be public-kb, seed or fixtures")
	case c.KBAllowDraft && !devLike:
		return fmt.Errorf("KB_ALLOW_DRAFT is local/dev only: production content must pass two-person approval")
	case c.SeedOnStart && !devLike:
		return fmt.Errorf("PUBLIC_AI_SEED_ON_START is dev/test only")
	}
	if err := validateRetention(c); err != nil {
		return err
	}
	if c.SessionTTL <= 0 || c.IdempotencyTTL <= 0 || c.ModelTimeout <= 0 {
		return fmt.Errorf("TTL and timeout values must be positive")
	}
	return nil
}

// validateRetention: outside dev/test the retention period must come from the
// signed policy, never from a default (Master prompt §10).
func validateRetention(c Config) error {
	if c.Environment != "dev" && c.Environment != "test" && !c.RetentionSet {
		return fmt.Errorf("PUBLIC_AI_RETENTION must be set from the signed retention policy outside dev/test (TODO(DPO): DEC-06)")
	}
	return nil
}

// envDefaultDev is true when PUBLIC_AI_ENVIRONMENT is dev or test (unset = dev).
func envDefaultDev() bool {
	e := env("PUBLIC_AI_ENVIRONMENT", "dev")
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
	return "mock-extractive-v1"
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
