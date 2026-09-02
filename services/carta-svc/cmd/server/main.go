package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"zoiko.io/carta-svc/internal/authz"
	"zoiko.io/carta-svc/internal/handler"
	"zoiko.io/carta-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8142"
	}

	s := store.NewMemoryStore()
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
	h := handler.New(s, authzClient, logger)
	router := handler.NewRouter(h)

	srv := &http.Server{Addr: ":" + port, Handler: router, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("carta-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
