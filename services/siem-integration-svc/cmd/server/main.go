package main

import (
	"context"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"
	"zoiko.io/siem-integration-svc/internal/handler"
	"zoiko.io/siem-integration-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" {
		port = "8141"
	}

	s := store.NewMemoryStore()
	h := handler.New(s, logger)
	router := handler.NewRouter(h)

	srv := &http.Server{Addr: ":" + port, Handler: router, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("siem-integration-svc listening on :" + port)
	quit := make(chan os.Signal, 1)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
