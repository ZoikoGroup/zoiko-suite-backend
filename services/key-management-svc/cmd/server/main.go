package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
	apicontract "zoiko.io/identity-envelope/apicontract"

	"zoiko.io/key-management-svc/internal/authz"
	"zoiko.io/key-management-svc/internal/config"
	kmscrypto "zoiko.io/key-management-svc/internal/crypto"
	"zoiko.io/key-management-svc/internal/events"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/idempotency"
	"zoiko.io/key-management-svc/internal/outbox"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	// Config fails on a missing DATABASE_URL or AUTHZ_SERVICE_URL: key
	// metadata and version history are authoritative and live only in
	// Postgres, so there is no in-memory fallback to start with.
	cfg, err := config.Load()
	if err != nil {
		logger.Fatal("invalid configuration", zap.Error(err))
	}
	port := cfg.Port

	// Durable storage: key metadata, version history and the token vault are
	// stored in Postgres only. config.Load already refused a missing
	// DATABASE_URL; a database that is unreachable at boot is fatal too, for
	// the same reason: silently starting without it would turn a deployment
	// mistake into lost tokenized bank account numbers.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		logger.Fatal("DATABASE_URL is not a valid Postgres connection string", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := pgxpool.NewWithConfig(connectCtx, poolCfg)
	if err == nil {
		err = pool.Ping(connectCtx)
	}
	cancelConnect()
	if err != nil {
		logger.Fatal("cannot connect to Postgres", zap.Error(err))
	}
	defer pool.Close()
	s := store.NewPgStore(pool)

	// The vault's encryption key. Fatal when missing or malformed: a vault
	// that started without one could only refuse every tokenize, or worse.
	vaultCipher, err := kmscrypto.New(os.Getenv(kmscrypto.EnvVarMasterKey))
	if err != nil {
		logger.Fatal("token vault key invalid", zap.Error(err))
	}

	siemClient := siem.New(os.Getenv("SIEM_SERVICE_URL"), "key-management-svc", logger)

	// Drain accepted SIEM events on shutdown. Stream returns before delivery,
	// so without this a SIGTERM would discard security events already accepted.
	defer siemClient.Close()
	// Fail fast rather than starting and 503-ing every guarded route.
	// authz.NewClient("") builds requests against an empty base URL, so every
	// CheckAllowed fails and the client — correctly — refuses. That posture is
	// right at request time but wrong at boot: a service that starts and then
	// rejects all authorized traffic hides a deployment mistake until a user
	// finds it. This dependency was added in Priority 2b and was missing from
	// docker-compose.yml for two services, which is exactly how that happens.
	// (config.Load enforces it.)
	authzClient := authz.NewClient(cfg.AuthzServiceURL)
	h := handler.New(s, siemClient, authzClient, logger).
		WithVault(s, vaultCipher).
		WithIdempotency(idempotency.NewPgStore(pool)).
		WithReadiness(pool)
	router := handler.NewRouter(h)

	// Transactional outbox relay: lifecycle events are written with the
	// change in one transaction; the relay delivers them to Kafka.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()
	if cfg.KafkaBrokers != "" {
		publisher, closeKafka := events.NewKafkaPublisher(cfg.KafkaBrokers, cfg.KafkaTopic)
		defer func() { _ = closeKafka() }()
		go outbox.NewRelay(pool, publisher, 2*time.Second, 50, logger).Start(relayCtx)
	} else {
		logger.Warn("KAFKA_BROKERS is not set: the outbox relay is not running; events are kept in outbox_events and delivered once it is configured",
			zap.String("topic", cfg.KafkaTopic))
	}

	// Wrap router with apicontract middleware
	r := chi.NewRouter()
	r.Use(apicontract.RequestID)
	r.Use(apicontract.DefaultCache)
	r.Mount("/", router)

	srv := &http.Server{Addr: ":" + port, Handler: r, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("key-management-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
