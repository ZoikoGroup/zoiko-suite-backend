// Package config loads the public-ai-svc runtime configuration from
// environment variables (IMPLEMENTATION_PROMPT §7, Doc 04 App. B).
//
// Every numeric limit here is a DEV DEFAULT: Docs 05/06/08 deliberately leave
// TTLs, rate limits and spend ceilings to be ratified (DEC-07, DEC-08, D06-05).
package config

import (
	"fmt"
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
	Retention                 time.Duration
	// Environment is dev, test, staging or production. Outside dev/test the
	// retention period must come from the signed policy config, never from a
	// default (Master prompt §10): RetentionSet records whether it was given.
	Environment  string
	RetentionSet bool
	MaxBodyBytes              int64
	ModelTimeout              time.Duration
	RateLimitPerIPPerMinute   int
	RateLimitPerSessionPerMin int
	DailyTokenCeiling         int64
	MonthlyTokenCeiling       int64

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
		Enabled:                   envBool("PUBLIC_AI_ENABLED", false),
		HTTPAddr:                  env("PUBLIC_AI_HTTP_ADDR", ":8080"),
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
		MaxBodyBytes:              int64(envInt("PUBLIC_AI_MAX_BODY_BYTES", 16*1024)),
		ModelTimeout:              envDuration("PUBLIC_AI_MODEL_TIMEOUT", 6*time.Second),
		RateLimitPerIPPerMinute:   envInt("PUBLIC_AI_RATE_LIMIT_IP_PER_MIN", 60),
		RateLimitPerSessionPerMin: envInt("PUBLIC_AI_RATE_LIMIT_SESSION_PER_MIN", 20),
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
	if c.Environment != "dev" && c.Environment != "test" && !c.RetentionSet {
		return fmt.Errorf("PUBLIC_AI_RETENTION must be set from the signed retention policy outside dev/test (TODO(DPO): DEC-06)")
	}
	if c.SessionTTL <= 0 || c.IdempotencyTTL <= 0 || c.ModelTimeout <= 0 {
		return fmt.Errorf("TTL and timeout values must be positive")
	}
	return nil
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
