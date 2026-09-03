package main

import (
	"context"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"zoiko.io/mtls-management-svc/internal/authz"
	"zoiko.io/mtls-management-svc/internal/ca"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/siem"
	"zoiko.io/mtls-management-svc/internal/store"
)

// loadBootstrapToken reads the shared mTLS self-provisioning bootstrap
// secret from a file (see deployments/docker-compose.yml's
// mtls-bootstrap-keygen init container) rather than an env var, so the
// value never appears in `docker inspect`/process-list output the way a
// plain environment variable would. Returns "" (bootstrap path disabled)
// when the path is unset or unreadable — this service must still start
// and serve the normal human/admin-authorized provisioning path even if
// the bootstrap file was never mounted.
func loadBootstrapToken(logger *zap.Logger) string {
	path := os.Getenv("MTLS_BOOTSTRAP_TOKEN_PATH")
	if path == "" {
		logger.Info("MTLS_BOOTSTRAP_TOKEN_PATH not set — self-provisioning bootstrap path disabled")
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Warn("failed to read mtls bootstrap token file — self-provisioning bootstrap path disabled",
			zap.String("path", path), zap.Error(err))
		return ""
	}
	return strings.TrimSpace(string(data))
}

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8140"
	}

	_ = chi.NewRouter()
	_ = chimw.RequestID

	caDir := os.Getenv("CA_DATA_DIR")
	if caDir == "" {
		caDir = "/data/ca"
	}
	internalCA, err := ca.LoadOrCreate(caDir)
	if err != nil {
		logger.Fatal("failed to load or create internal CA", zap.String("ca_data_dir", caDir), zap.Error(err))
	}
	logger.Info("internal CA ready", zap.String("ca_data_dir", caDir))

	s := store.NewMemoryStore()
	siemClient := siem.New(os.Getenv("SIEM_SERVICE_URL"), "mtls-management-svc", logger)
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
	bootstrapToken := loadBootstrapToken(logger)
	h := handler.New(s, internalCA, siemClient, authzClient, logger, bootstrapToken)
	router := handler.NewRouter(h)

	srv := &http.Server{Addr: ":" + port, Handler: router, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("mtls-management-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
