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
	"syscall"
	"time"

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
	"zoiko.io/public-ai-svc/internal/release"
	"zoiko.io/public-ai-svc/internal/session"
	"zoiko.io/public-ai-svc/internal/store"
)

const refreshInterval = 5 * time.Second

// gitSHA is the application artifact version, set at build time
// (-ldflags "-X main.gitSHA=...") and recorded in the release manifest.
var gitSHA = "unknown"

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

	var (
		st  store.Store
		rt  *release.Runtime
		fl  *flags.Loader
		pub events.Publisher = events.LogPublisher{Log: log}
	)
	if cfg.DatabaseURL == "" {
		log.Warn("LOCAL DEV MODE: no PUBLIC_AI_DATABASE_URL; serving TEST-ONLY fixtures from memory")
		env, err := fixtureenv.New(ctx, cfg.ModelTimeout)
		if err != nil {
			log.Fatal("dev fixtures", zap.Error(err))
		}
		st, rt, fl = env.Store, env.Releases, env.Flags
	} else {
		st = openPostgres(ctx, log, cfg)
		if cfg.SigningPublicKey == "" {
			log.Fatal("PUBLIC_AI_SIGNING_PUBLIC_KEY is required to verify releases")
		}
		rt = release.New(st, cfg.SigningPublicKey)
		fl = flags.NewLoader(st)
		if len(cfg.KafkaBrokers) > 0 {
			pub = events.NewKafkaPublisher(cfg.KafkaBrokers)
		}
	}
	defer pub.Close() //nolint:errcheck

	// Initial load; failures leave the service not-ready (fail closed).
	if err := rt.Refresh(ctx); err != nil {
		log.Error("release load failed; service is not ready", zap.Error(err))
	}
	if err := fl.Refresh(ctx); err != nil {
		log.Error("flag load failed; assistant disabled", zap.Error(err))
	}
	go rt.Run(ctx, refreshInterval, func(err error) { log.Error("release refresh failed", zap.Error(err)) })
	go runEvery(ctx, refreshInterval, func() {
		if err := fl.Refresh(ctx); err != nil {
			log.Error("flag refresh failed; assistant disabled", zap.Error(err))
		}
	})
	relay := &events.Relay{Store: st, Pub: pub, Log: log}
	go relay.Run(ctx, time.Second)

	pipeline := &orchestrator.Pipeline{
		Releases: rt, Flags: fl, Store: st, Log: log, MaxOutputTokens: cfg.TokenBudgetPerTurn / 4,
		Model: model.Gateway{Provider: providerFor(log, cfg), Timeout: cfg.ModelTimeout},
	}
	guard := limits.New(cfg, st, fl)
	api := httpapi.New(cfg, log, httpapi.Deps{
		Store: st, Sessions: session.New(st, cfg.SessionTTL), Answerer: guard.Wrap(pipeline), Config: rt,
		Limiter: guard.Middleware,
		Ready: func() bool {
			return cfg.Enabled && rt.Ready(3*refreshInterval) && fl.Healthy() && fl.Values().PublicAIEnabled
		},
	})
	serve(ctx, log, cfg, api.Router())
}

// providerFor returns the configured model provider. Only the Fake exists
// until a vendor, model and region are approved (DEC-05, ADR-003).
func providerFor(log *zap.Logger, cfg config.Config) model.Provider {
	if cfg.ModelProfile != "fake-v0" {
		log.Fatal("no approved model provider for profile", zap.String("profile", cfg.ModelProfile))
	}
	return &model.Fake{}
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

func serve(ctx context.Context, log *zap.Logger, cfg config.Config, h http.Handler) {
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
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Error("forced shutdown", zap.Error(err))
	}
	_ = os.Stdout.Sync()
}
