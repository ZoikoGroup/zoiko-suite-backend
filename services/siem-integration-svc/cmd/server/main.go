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

	"zoiko.io/siem-integration-svc/internal/authz"
	"zoiko.io/siem-integration-svc/internal/credential"
	"zoiko.io/siem-integration-svc/internal/handler"
	"zoiko.io/siem-integration-svc/internal/idempotency"
	"zoiko.io/siem-integration-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8141"
	}

	// Exporters and the security event stream are authoritative state:
	// Postgres, never the in-memory map (unit tests only). The pool connects
	// lazily, so a database outage shows as /readyz 503, not a crash loop.
	pool, err := newPool(databaseURL())
	if err != nil {
		logger.Fatal("invalid DATABASE_URL", zap.Error(err))
	}
	defer pool.Close()

	// Exporter credentials are sealed at rest (internal/credential). Without
	// a key, exporters can still be created, but one carrying an auth_token
	// is refused (503 credential_sealing_unavailable) — never stored in
	// plaintext. Key material is never logged.
	keys, err := credential.ParseKeys(os.Getenv("SIEM_CREDENTIAL_KEY_ID"),
		os.Getenv("SIEM_CREDENTIAL_KEY"), os.Getenv("SIEM_CREDENTIAL_PREVIOUS_KEYS"))
	if err != nil {
		logger.Fatal("invalid SIEM_CREDENTIAL_KEY configuration", zap.Error(err))
	}
	if keys == nil {
		logger.Warn("SIEM_CREDENTIAL_KEY unset: exporters carrying an auth_token will be refused")
	} else {
		logger.Info("exporter credential sealing enabled", zap.String("key_id", keys.CurrentKeyID()))
	}
	s := store.NewPgStore(pool, keys)
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
	h := handler.New(s, authzClient, logger).WithIdempotency(idempotency.NewPgStore(pool))
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
	logger.Info("siem-integration-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// defaultDatabaseURL matches the sibling services' compose wiring.
const defaultDatabaseURL = "postgres://postgres:postgres@postgres:5432/siem_integration?sslmode=disable"

func databaseURL() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return defaultDatabaseURL
}

// newPool builds the pgx pool without dialling.
func newPool(dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = time.Minute
	return pgxpool.NewWithConfig(context.Background(), cfg)
}
