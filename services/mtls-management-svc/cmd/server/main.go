package main

import (
	"net/http"
	"os"
	"context"
	"time"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/mtls-management-svc/internal/handler"
	"zoiko.io/mtls-management-svc/internal/store"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()
	port := os.Getenv("PORT")
	if port == "" { port = "8140" }

	_ = chi.NewRouter()
	_ = chimw.RequestID

	s := store.NewMemoryStore()
	h := handler.New(s, logger)
	router := handler.NewRouter(h)

	srv := &http.Server{Addr: ":" + port, Handler: router, ReadTimeout: 15*time.Second, WriteTimeout: 15*time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("server error", zap.Error(err))
		}
	}()
	logger.Info("mtls-management-svc listening on :"+port)
	quit := make(chan os.Signal, 1)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
