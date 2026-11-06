// Command server runs the public-ai-svc HTTP API.
//
// With PUBLIC_AI_DATABASE_URL set it uses Postgres, loads the promoted signed
// releases (verified with PUBLIC_AI_SIGNING_PUBLIC_KEY) and relays events to
// Kafka (or the log when PUBLIC_AI_KAFKA_BROKERS is empty).
//
// Without a database it starts LOCAL DEV MODE: in-memory store and the
// TEST-ONLY fixtures signed with a throwaway key. Never use that mode for
// anything but local development.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
	"zoiko.io/public-ai-svc/internal/metrics"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"zoiko.io/public-ai-svc/deployments/migrations"
	"zoiko.io/public-ai-svc/internal/config"
	"zoiko.io/public-ai-svc/internal/events"
	"zoiko.io/public-ai-svc/internal/fixtureenv"
	"zoiko.io/public-ai-svc/internal/flags"
	"zoiko.io/public-ai-svc/internal/httpapi"
	"zoiko.io/public-ai-svc/internal/limits"
	"zoiko.io/public-ai-svc/internal/model"
	"zoiko.io/public-ai-svc/internal/orchestrator"
	"zoiko.io/public-ai-svc/internal/policy"
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
	"zoiko.io/public-ai-svc/internal/telemetry"
)

const refreshInterval = 5 * time.Second

// gitSHA is the application artifact version, set at build time
// (-ldflags "-X main.gitSHA=...") and recorded in the release manifest.
var gitSHA = "unknown"

// draining is set on SIGTERM: readiness turns false so the load balancer
// stops routing new turns while in-flight ones finish (Doc 08 §11).
var draining atomic.Bool

// started is set once the first release and flag load completed.
var started atomic.Bool

// envKillSwitch folds PUBLIC_AI_ENABLED into public_ai_enabled, so a
// deployment-level off is served as the typed KILL_SWITCH outcome exactly
// like the runtime flag instead of failing readiness (Doc 08 §26, OPR-011).
type envKillSwitch struct {
	fl      *flags.Loader
	enabled bool
}

func (e envKillSwitch) Values() flags.Values {
	v := e.fl.Values()
	if !e.enabled {
		v.PublicAIEnabled = false
	}
	return v
}

// readiness is release-runtime readiness: not draining, verified releases
// fresh and flags readable. The kill switch is deliberately absent.
func readiness(releasesReady func() bool, flagsHealthy func() bool) func() bool {
	return func() bool { return !draining.Load() && releasesReady() && flagsHealthy() }
}

// drainGrace is how long readiness reports false before the listener stops.
const drainGrace = config.DrainGrace

// shutdownTimeout always outlasts a turn's lease (2 x model timeout + 5 s,
// httpapi), so an in-flight turn ends with a stored outcome (OPR-012).
// config bounds the model timeout so drain + this fits the stop grace.
func shutdownTimeout(cfg config.Config) time.Duration {
	return 2*cfg.ModelTimeout + config.ShutdownSlack
}

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		panic(err)
	}
	defer log.Sync() //nolint:errcheck

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("invalid configuration", zap.Error(err))
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownTracing, err := telemetry.Setup(ctx, cfg.Environment)
	if err != nil {
		log.Fatal("tracing setup failed", zap.Error(err))
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(sctx)
	}()
	log.Info("tracing", zap.Bool("otlp_export", telemetry.Enabled()))
	// Doc 06 §08: only the approved policy runs.
	if err := policy.CheckDigest(cfg.PolicyDigest, model.PromptTemplateVersion); err != nil {
		log.Fatal("policy digest", zap.Error(err))
	}

	var (
		st  store.Store
		rt  *release.Runtime
		fl  *flags.Loader
		pub events.Publisher = events.LogPublisher{Log: log}
	)
	if cfg.DatabaseURL == "" {
		if cfg.Environment != "dev" && cfg.Environment != "test" {
			log.Fatal("PUBLIC_AI_DATABASE_URL is required outside dev/test")
		}
		log.Warn("LOCAL DEV MODE: no PUBLIC_AI_DATABASE_URL; serving SYNTHETIC data from memory",
			zap.String("store", "memory"), zap.String("dataset", cfg.Dataset), zap.String("model_provider", cfg.ModelProvider), zap.Bool("kb_allow_draft", cfg.KBAllowDraft),
			zap.String("retrieval", "lexical BM25 + aliases; no embeddings configured (ADR-002)"))
		env, err := fixtureenv.NewFromDirWith(ctx, fixtureenv.DatasetDir(cfg.Dataset), store.NewMemory(), "dev-1", cfg.ModelTimeout, cfg.KBAllowDraft)
		if err != nil {
			log.Fatal("dev dataset", zap.Error(err))
		}
		st, rt, fl = env.Store, env.Releases, env.Flags
	} else if cfg.SeedOnStart {
		// Dev/test only (config enforces): migrate -> seed -> sign and publish
		// -> move the active pointers, then serve with that release's key.
		st = openPostgres(ctx, log, cfg)
		env, err := fixtureenv.NewFromDirWith(ctx, fixtureenv.DatasetDir(cfg.Dataset), st, "seed-"+time.Now().UTC().Format("20060102T150405"), cfg.ModelTimeout, cfg.KBAllowDraft)
		if err != nil {
			log.Fatal("seed on start", zap.Error(err))
		}
		log.Warn("SEED ON START: published and activated the SYNTHETIC dataset", zap.String("dataset", cfg.Dataset))
		rt, fl = env.Releases, env.Flags
	} else {
		st = openPostgres(ctx, log, cfg)
		if cfg.SigningPublicKey == "" {
			log.Fatal("PUBLIC_AI_SIGNING_PUBLIC_KEY is required to verify releases")
		}
		rt = release.New(st, cfg.SigningPublicKey)
		rt.AllowDraft = cfg.KBAllowDraft // config refuses it outside dev/test
		// OPR-004: a release names the policy versions it was evaluated with.
		rt.PolicyVersion = cfg.PolicyVersion
		rt.RequireCompatibility = cfg.Environment != "dev" && cfg.Environment != "test"
		fl = flags.NewLoader(st)
		if len(cfg.KafkaBrokers) > 0 {
			pub = events.NewKafkaPublisher(cfg.KafkaBrokers)
		}
	}
	defer pub.Close() //nolint:errcheck

	if cfg.StructuredResponseDev && (cfg.DatabaseURL == "" || cfg.SeedOnStart) {
		// Dev/test only (config enforces); recorded like any other flag change.
		if err := flags.Set(ctx, st, flags.StructuredResponseEnabled, []byte("true"), "local-dev", "PUBLIC_AI_STRUCTURED_RESPONSE", cfg.Environment); err != nil {
			log.Fatal("structured response flag", zap.Error(err))
		}
		log.Warn("structured responses on (dev): GET /menu and turn followups/cta/outcome")
	}
	// Initial load; failures leave the service not-ready (fail closed).
	// /health/startup turns true once the first release and flag load has
	// completed (Doc 08 §10), whatever its result: readiness reports health.
	if err := rt.Refresh(ctx); err != nil {
		log.Error("release load failed; service is not ready", zap.Error(err))
	}
	if err := fl.Refresh(ctx); err != nil {
		log.Error("flag load failed; assistant disabled", zap.Error(err))
	}
	started.Store(true)
	go rt.Run(ctx, refreshInterval, func(err error) { log.Error("release refresh failed", zap.Error(err)) })
	go runEvery(ctx, refreshInterval, func() {
		if err := fl.Refresh(ctx); err != nil {
			log.Error("flag refresh failed; assistant disabled", zap.Error(err))
		}
	})
	// Retention: purge expired/deleted sessions and their turns (DEC-06 pending).
	go runEvery(ctx, 10*time.Minute, func() {
		if n, err := st.PurgeSessions(ctx, time.Now().Add(-cfg.Retention)); err != nil {
			log.Error("retention purge failed", zap.Error(err))
		} else if n > 0 {
			log.Info("retention purge", zap.Int("sessions", n))
		}
	})
	relay := &events.Relay{Store: st, Pub: pub, Log: log}
	go relay.Run(ctx, time.Second)

	kill := envKillSwitch{fl: fl, enabled: cfg.Enabled}
	pipeline := &orchestrator.Pipeline{
		Releases: rt, Flags: kill, Store: st, Log: log, MaxOutputTokens: cfg.TokenBudgetPerTurn / 4,
		// Release tuple on every decision record (Doc 08 §06).
		PolicyVersion: cfg.PolicyVersion, ModelID: cfg.ModelID, AppVersion: gitSHA,
		Model: model.Gateway{Provider: providerFor(log, cfg), Timeout: cfg.ModelTimeout, ModelID: cfg.ModelID, Breaker: &model.Breaker{
			Threshold: 5, Cooldown: 30 * time.Second,
			OnOpen: func() {
				log.Error("ALERT model circuit open: provider failing; serving UNAVAILABLE with official routes")
			},
		}},
	}
	guard := limits.New(cfg, st, fl)
	// SEC-T046: idle rate-limit buckets are dropped, so no visitor key lives
	// past its window (Doc 06 §17).
	go guard.RunSweeper(ctx, time.Minute)
	api := httpapi.New(cfg, log, httpapi.Deps{
		Store: st, Sessions: session.New(st, cfg.SessionTTL).WithIdleTTL(cfg.SessionIdleTTL).WithIdempotencyTTL(cfg.IdempotencyTTL), Answerer: guard.Wrap(pipeline), Config: rt, Routes: rt, Flags: kill,
		Limiter:      guard.Middleware,
		Ready:        readiness(func() bool { return rt.Ready(3 * refreshInterval) }, fl.Healthy),
		ReleaseReady: func() bool { return rt.Ready(3 * refreshInterval) },
		Started:      started.Load,
		ChatEnabled:  func() bool { return kill.Values().PublicAIEnabled },
	})
	metrics.SetBuildInfo(gitSHA, cfg.ModelProfile)
	serve(ctx, log, cfg, api.Router(), api.AdminRouter())
}

// providerFor returns the configured model provider behind the same
// model.Provider interface (no tools): the deterministic mock (dev/test) or
// Groq. Config has already refused unknown providers, a missing key and any
// model id outside the allowlist. The key is never logged.
func providerFor(log *zap.Logger, cfg config.Config) model.Provider {
	switch cfg.ModelProvider {
	case "groq":
		log.Info("model provider", zap.String("provider", "groq"), zap.String("model", cfg.ModelID), zap.String("prompt_version", model.PromptVersion))
		return &model.Groq{APIKey: cfg.GroqAPIKey, Model: cfg.ModelID}
	case "mock":
		return &model.Fake{}
	}
	log.Fatal("no approved model provider", zap.String("provider", cfg.ModelProvider))
	return nil
}

func openPostgres(ctx context.Context, log *zap.Logger, cfg config.Config) store.Store {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(cctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal("database config", zap.Error(err))
	}
	if err := pool.Ping(cctx); err != nil {
		log.Fatal("database ping", zap.Error(err))
	}
	if err := store.Migrate(cctx, pool, migrations.FS); err != nil {
		log.Fatal("migrations", zap.Error(err))
	}
	log.Info("storage ready", zap.String("store", "postgres"))
	return store.NewPostgres(pool)
}

func runEvery(ctx context.Context, d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

func serve(ctx context.Context, log *zap.Logger, cfg config.Config, h, admin http.Handler) {
	// Doc 06 §12: /metrics and /health/* on an internal listener when
	// PUBLIC_AI_ADMIN_ADDR is set; the public listener then has none.
	var adminSrv *http.Server
	if cfg.AdminAddr != "" {
		adminSrv = &http.Server{Addr: cfg.AdminAddr, Handler: admin, ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
		go func() {
			log.Info("admin listener (metrics, health)", zap.String("addr", cfg.AdminAddr))
			if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatal("admin server failed", zap.Error(err))
			}
		}()
	}
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      2*cfg.ModelTimeout + 15*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		log.Info("public-ai-svc listening", zap.String("addr", cfg.HTTPAddr), zap.Bool("enabled", cfg.Enabled), zap.String("git_sha", gitSHA))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("http server failed", zap.Error(err))
		}
	}()
	<-ctx.Done()
	draining.Store(true)
	log.Info("draining: readiness false", zap.Duration("grace", drainGrace))
	time.Sleep(drainGrace)
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout(cfg))
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("forced shutdown", zap.Error(err))
	}
	if adminSrv != nil {
		_ = adminSrv.Shutdown(sctx)
	}
	_ = os.Stdout.Sync()
}
