// Command server is the entrypoint for gateway-auth-svc — the ForwardAuth
// target Traefik calls before routing any gated request to a backend
// service. Stateless: no database, no message broker, just JWT/JWKS
// verification.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"zoiko.io/gateway-auth-svc/internal/carta"
	"zoiko.io/gateway-auth-svc/internal/config"
	"zoiko.io/gateway-auth-svc/internal/handler"
	"zoiko.io/gateway-auth-svc/internal/jwks"
	"zoiko.io/gateway-auth-svc/internal/router"
	"zoiko.io/gateway-auth-svc/internal/siem"
	"zoiko.io/gateway-auth-svc/internal/tenantctx"
)

func main() {
	log, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialise logger: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config load failed", zap.Error(err))
	}

	jwksClient := jwks.NewClient(cfg.JWKSURL, cfg.JWKSCacheTTL)
	cartaClient := carta.New(cfg.CartaServiceURL, log)
	siemClient := siem.New(cfg.SIEMServiceURL, "gateway-auth-svc", log)

	// GOV-01 tenant context resolution against tenant-entity-registry-svc.
	// nil when TENANT_REGISTRY_URL is unset, which leaves the gateway behaving
	// exactly as before rather than failing closed on an unconfigured dependency.
	tenantResolver := tenantctx.New(cfg.TenantRegistryURL, cfg.TenantContextTTL, cfg.TenantContextStaleGrace)
	if tenantResolver.Enabled() {
		log.Info("tenant context resolution enabled",
			zap.String("registry", cfg.TenantRegistryURL),
			zap.Duration("ttl", cfg.TenantContextTTL),
			zap.Duration("stale_grace", cfg.TenantContextStaleGrace))
	} else {
		log.Warn("tenant context resolution DISABLED — set TENANT_REGISTRY_URL to enable GOV-01 " +
			"resolution; tenant operability and cross-tenant entity ownership are not checked at the gateway")
	}

	h := handler.New(cfg, jwksClient, cartaClient, siemClient, tenantResolver, log)
	r := router.New(h, jwksClient)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Info("gateway-auth-svc starting", zap.Int("port", cfg.Port))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining connections")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("gateway-auth-svc stopped")
}
