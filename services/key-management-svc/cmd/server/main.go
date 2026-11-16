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
	kmscrypto "zoiko.io/key-management-svc/internal/crypto"
	"zoiko.io/key-management-svc/internal/handler"
	"zoiko.io/key-management-svc/internal/siem"
	"zoiko.io/key-management-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8143"
	}

	// Durable storage, for the first time in this service. It used to run on
	// store.NewMemoryStore(), which forgot every key registration — and would
	// now forget every tokenized bank account number — on each restart.
	// Required, and fatal when absent, for the same reason AUTHZ_SERVICE_URL
	// is: there is no safe default, and silently falling back to memory would
	// turn a missing variable into data loss.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		logger.Fatal("DATABASE_URL is required: key metadata and the token vault are stored in Postgres")
	}
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		logger.Fatal("DATABASE_URL is not a valid Postgres connection string", zap.Error(err))
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 10*time.Second)
	dbpool, err := pgxpool.NewWithConfig(connectCtx, poolCfg)
	if err == nil {
		err = dbpool.Ping(connectCtx)
	}
	cancelConnect()
	if err != nil {
		logger.Fatal("cannot connect to Postgres", zap.Error(err))
	}
	defer dbpool.Close()
	s := store.NewPgStore(dbpool)

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
	authzURL := os.Getenv("AUTHZ_SERVICE_URL")
	if authzURL == "" {
		logger.Fatal("AUTHZ_SERVICE_URL is required: every authorized route would return 503 without it")
	}
	authzClient := authz.NewClient(authzURL)
	h := handler.New(s, siemClient, authzClient, logger).WithVault(s, vaultCipher)
	router := handler.NewRouter(h)

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
