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
	idenv "zoiko.io/identity-envelope"

	"zoiko.io/gateway-auth-svc/internal/admin"
	"zoiko.io/gateway-auth-svc/internal/admission"
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
	// tenant-entity-registry-svc. Off unless the peer's *_MTLS_ENABLED is
	// "true", in which case config.Load has already pointed that peer's URL at
	// its https:// mTLS listener. One workload identity serves both peers.
	var jwksHTTPClient, tenantRegistryHTTPClient *http.Client
	if cfg.IdentityJWKSMTLSEnabled || cfg.TenantRegistryMTLSEnabled {
		// The synthetic platform-scope legal entity that owns platform-wide
		// reference data; the same scope other services provision under.
		const platformScopeID = "00000000-0000-0000-0000-00000000f001"
		client, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL,
			"gateway-auth-svc", platformScopeID, cfg.MTLSBootstrapTokenPath)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		if cfg.IdentityJWKSMTLSEnabled {
			jwksHTTPClient = client
			log.Info("mTLS enabled for identity-context-svc JWKS calls", zap.String("url", cfg.JWKSURL))
		}
		if cfg.TenantRegistryMTLSEnabled {
			tenantRegistryHTTPClient = client
			log.Info("mTLS enabled for tenant-entity-registry-svc calls", zap.String("url", cfg.TenantRegistryURL))
		}
	}

	// Plain clients for a peer whose mTLS is off. Both constructors keep the
	// client exactly as given, so the nil left above for a non-mTLS peer
	// panicked on the first /verify (found live, 14 Oct 2026): every gated
	// request answered 500 on any deployment with IDENTITY_JWKS_MTLS_ENABLED
	// unset, and every tenant resolution would have done the same with
	// TENANT_REGISTRY_URL set and TENANT_REGISTRY_MTLS_ENABLED unset.
	if jwksHTTPClient == nil {
		jwksHTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	if tenantRegistryHTTPClient == nil {
		tenantRegistryHTTPClient = &http.Client{Timeout: 5 * time.Second}
	}

	jwksClient := jwks.NewClientWithHTTPClient(cfg.JWKSURL, cfg.JWKSCacheTTL, jwksHTTPClient)
	cartaClient := carta.New(cfg.CartaServiceURL, log)
	siemClient := siem.New(cfg.SIEMServiceURL, "gateway-auth-svc", log)

	// Drain accepted SIEM events on shutdown. Stream returns before delivery,
	// so without this a SIGTERM would discard security events already accepted.
	defer siemClient.Close()
	metrics.RegisterSIEMDrops(siemClient.Dropped, "gateway-auth-svc")

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

	// AU-01: a verified envelope's session is checked at identity-context-svc
	// (live answers reused for the revocation TTL), so a logout stops passing
	// the edge at once instead of when the envelope expires.
	if cfg.IdentityRevocation == "on" {
		fail := idenv.FailClosed
		if cfg.RevocationFail == "open" {
			fail = idenv.FailOpen
		}
		h.UseRevocation(idenv.NewRevocation(idenv.RevocationOptions{
			Source:    idenv.NewIntrospector(cfg.IdentityIntrospectURL, jwksHTTPClient),
			ActiveTTL: cfg.RevocationTTL,
			Fail:      fail,
		}))
		log.Info("identity session revocation check on", zap.String("introspect_url", cfg.IdentityIntrospectURL),
			zap.Duration("ttl", cfg.RevocationTTL), zap.String("fail", string(fail)))
	} else {
		log.Warn("IDENTITY_REVOCATION=off — a revoked session keeps passing the edge until its envelope expires (ENV=local only)")
	}

	// INT-01 admission: quota, request-shape and version admission, overload
	// shedding, and the bounded record of refusals. All in-process; none adds a
	// dependency to the verification path.
	adm := handler.Admission{
		Policy: admission.Policy{
			MaxBodyBytes:       cfg.MaxBodyBytes,
			AllowedEncodings:   cfg.AllowedRequestEncodings,
			MaxCompressedBytes: cfg.MaxCompressedBodyBytes,
			SupportedVersions:  cfg.SupportedAPIVersions,
		},
		IPLimiter:     admission.NewLimiter(cfg.RateIPPerSecond, cfg.RateIPBurst, 0, nil),
		TenantLimiter: admission.NewLimiter(cfg.RateTenantPerSecond, cfg.RateTenantBurst, 0, nil),
		Gate:          admission.NewGate(cfg.MaxInFlight),
		Recorder:      admission.NewRecorder(cfg.DecisionWindow, cfg.AbuseThresholdPerMinute, nil),
	}
	log.Info("gateway admission configured",
		zap.Int64("max_body_bytes", cfg.MaxBodyBytes),
		zap.Strings("allowed_request_encodings", cfg.AllowedRequestEncodings),
		zap.Strings("supported_api_versions", cfg.SupportedAPIVersions),
		zap.Float64("rate_ip_per_second", cfg.RateIPPerSecond),
		zap.Float64("rate_tenant_per_second", cfg.RateTenantPerSecond),
		zap.Int("max_in_flight", cfg.MaxInFlight))
	if !adm.IPLimiter.Enabled() || !adm.TenantLimiter.Enabled() || cfg.MaxInFlight == 0 {
		log.Warn("a gateway admission protection is DISABLED by configuration (a limit of 0)")
	}

	h.UseAdmission(adm)
	r := router.New(h, jwksClient, metrics)

	// Read-only decision queries: a second port of this same service, served
	// only when both an address and a token file are configured (config.Load
	// refuses one without the other). The token comes from a file.
	var adminSrv *http.Server
	if cfg.AdminAddr != "" {
		token, err := admin.LoadToken(cfg.AdminTokenFile)
		if err != nil {
			log.Fatal("admin listener: cannot load the token", zap.Error(err))
		}
		handlerAdmin, err := admin.New(admin.Source{
			Recorder: adm.Recorder, IPLimiter: adm.IPLimiter, TenantLimiter: adm.TenantLimiter, Policy: adm.Policy,
		}, token)
		if err != nil {
			log.Fatal("admin listener: cannot start", zap.Error(err))
		}
		adminSrv = &http.Server{
			Addr: cfg.AdminAddr, Handler: handlerAdmin,
			ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		}
		go func() {
			log.Info("gateway decision query listener starting", zap.String("addr", cfg.AdminAddr))
			if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal("admin listener error", zap.Error(err))
			}
		}()
	}

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
	if adminSrv != nil {
		if err := adminSrv.Shutdown(shutdownCtx); err != nil {
			log.Error("admin listener shutdown failed", zap.Error(err))
		}
	}
	log.Info("gateway-auth-svc stopped")
}
