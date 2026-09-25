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
	"zoiko.io/gateway-auth-svc/internal/mtls"
	"zoiko.io/gateway-auth-svc/internal/router"
	"zoiko.io/gateway-auth-svc/internal/siem"
	"zoiko.io/gateway-auth-svc/internal/telemetry"
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

	// Tracing is best-effort: a collector that is down must not stop the
	// service every gated request in the estate depends on.
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "gateway-auth-svc", cfg.OTELExporterEndpoint)
	if err != nil {
		log.Warn("tracing disabled — OTLP exporter unavailable", zap.Error(err))
	} else {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTracing(shutdownCtx); err != nil {
				log.Warn("tracing shutdown failed", zap.Error(err))
			}
		}()
	}

	metrics := telemetry.NewMetrics("gateway-auth-svc")

	// mTLS for upstream calls to identity-context-svc (JWKS) and
	// tenant-entity-registry-svc. Disabled by default — plain HTTP is used
	// unless the corresponding MTLS_ENABLED env var is set to "true".
	var jwksHTTPClient, tenantRegistryHTTPClient *http.Client
	if cfg.IdentityJWKSMTLSEnabled {
		// The platform scope ID is the same one used by other services for
		// mTLS provisioning. It is the synthetic platform-scope legal entity
		// that owns platform-wide reference data.
		const platformScopeID = "00000000-0000-0000-0000-00000000f001"
		client, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "gateway-auth-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity for JWKS", zap.Error(err))
		}
		jwksHTTPClient = client
		log.Info("mTLS enabled for identity-context-svc JWKS calls", zap.String("url", cfg.IdentityJWKSMTLSURL))
	}
	if cfg.TenantRegistryMTLSEnabled {
		const platformScopeID = "00000000-0000-0000-0000-00000000f001"
		client, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "gateway-auth-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity for tenant registry", zap.Error(err))
		}
		tenantRegistryHTTPClient = client
		log.Info("mTLS enabled for tenant-entity-registry-svc calls", zap.String("url", cfg.TenantRegistryMTLSURL))
	}

	jwksClient := jwks.NewClientWithHTTPClient(cfg.JWKSURL, cfg.JWKSCacheTTL, jwksHTTPClient)
	cartaClient := carta.New(cfg.CartaServiceURL, log)
	siemClient := siem.New(cfg.SIEMServiceURL, "gateway-auth-svc", log)

	// Drain accepted SIEM events on shutdown. Stream returns before delivery,
	// so without this a SIGTERM would discard security events already accepted.
	defer siemClient.Close()

	// GOV-01 tenant context resolution against tenant-entity-registry-svc.
	// nil when TENANT_REGISTRY_URL is unset, which leaves the gateway behaving
	// exactly as before rather than failing closed on an unconfigured dependency.
	tenantResolver := tenantctx.NewWithHTTPClient(cfg.TenantRegistryURL, cfg.TenantContextTTL, cfg.TenantContextStaleGrace, tenantRegistryHTTPClient)
	if tenantResolver.Enabled() {
		log.Info("tenant context resolution enabled",
			zap.String("registry", cfg.TenantRegistryURL),
			zap.Duration("ttl", cfg.TenantContextTTL),
			zap.Duration("stale_grace", cfg.TenantContextStaleGrace))
	} else {
		log.Warn("tenant context resolution DISABLED — set TENANT_REGISTRY_URL to enable GOV-01 " +
			"resolution; tenant operability and cross-tenant entity ownership are not checked at the gateway")
	}

	h := handler.New(cfg, jwksClient, cartaClient, siemClient, tenantResolver, log).UseMetrics(metrics)
	r := router.New(h, jwksClient, metrics)

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
