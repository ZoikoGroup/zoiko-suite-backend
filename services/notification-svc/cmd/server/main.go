package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/riandyrn/otelchi"
	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"

	"zoiko.io/notification-svc/internal/authz"
	"zoiko.io/notification-svc/internal/config"
	"zoiko.io/notification-svc/internal/deliver"
	svcenvelope "zoiko.io/notification-svc/internal/envelope"
	"zoiko.io/notification-svc/internal/events"
	"zoiko.io/notification-svc/internal/handler"
	"zoiko.io/notification-svc/internal/health"
	"zoiko.io/notification-svc/internal/identity"
	svcmiddleware "zoiko.io/notification-svc/internal/middleware"
	"zoiko.io/notification-svc/internal/mtls"
	"zoiko.io/notification-svc/internal/retry"
	"zoiko.io/notification-svc/internal/store"
	"zoiko.io/notification-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — used as the legal_entity_id when this service provisions its own
// mTLS client identity from mtls-management-svc.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to load config: " + err.Error() + "\n")
		os.Exit(1)
	}

	// ── 2. Logger ─────────────────────────────────────────────────────────────
	log, err := zap.NewProduction()
	if err != nil {
		_, _ = os.Stderr.WriteString("fatal: failed to init logger: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer func() { _ = log.Sync() }()

	log.Info("notification-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
	)

	// ── 2b. Tracing ──────────────────────────────────────────────────────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "notification-svc", cfg.OTELExporterEndpoint)
	if err != nil {
		log.Fatal("otel tracing init failed", zap.Error(err))
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			log.Error("otel tracer provider shutdown failed", zap.Error(err))
		}
	}()

	metrics := telemetry.NewMetrics("notification-svc")

	// ── 3. Database pool ──────────────────────────────────────────────────────
	poolCfg, err := pgxpool.ParseConfig(cfg.DB.DSN())
	if err != nil {
		log.Fatal("failed to parse db pool config", zap.Error(err))
	}
	poolCfg.ConnConfig.Tracer = otelpgx.NewTracer()
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatal("failed to create db pool", zap.Error(err))
	}
	defer pool.Close()

	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4. Store, Kafka producer, clients ─────────────────────────────────────
	pgStore := store.New(pool)

	// A deployment says "no broker" with an explicitly empty KAFKA_BROKERS;
	// the publisher then logs instead of writing, rather than blocking every
	// send on a dial to a broker that does not exist.
	var kafkaWriter *kafka.Writer
	if len(cfg.Kafka.Brokers) > 0 {
		kafkaWriter = &kafka.Writer{
			Addr:     kafka.TCP(cfg.Kafka.Brokers...),
			Topic:    cfg.Kafka.Topic,
			Balancer: &kafka.LeastBytes{},
			// The broker has KAFKA_AUTO_CREATE_TOPICS_ENABLE=true, but kafka-go's
			// Writer refuses to produce to a not-yet-existing topic unless this
			// is also set client-side — without it, every first publish to this
			// service's own topic fails with "Unknown Topic Or Partition".
			AllowAutoTopicCreation: true,
			// kafka-go's default is 1s: a synchronous WriteMessages waits out
			// the full batch timeout before returning, so every notification
			// send paid a wasted second inside the request. Platform-wide
			// value, same as the other gap-closed services.
			BatchTimeout: 10 * time.Millisecond,
		}
		defer func() { _ = kafkaWriter.Close() }()
	} else {
		log.Warn("KAFKA_BROKERS is empty — events will be logged, not published")
	}

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)
	// The decision cache main.go grew for this client now lives in the authz
	// package alongside the client itself, so it is covered by client_test.go.
	var authzClient *authz.Client
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "notification-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, mtlsHTTPClient, log)
	} else {
		authzClient = authz.NewClient(cfg.AuthZServiceURL, log)
	}

	// identity-context-svc owns principals and their contact facts. An EMAIL
	// notification names its recipient by principal id, so without this client
	// there is no address to deliver to — which is the state this service
	// shipped in, covered by a stub adapter that reported success anyway.
	identityClient := identity.NewClient(cfg.IdentityServiceURL, log)

	// Mail provider. Nil when unconfigured, and the router then records EMAIL
	// as FAILED naming the missing provider rather than claiming a send.
	//
	// Constructed before the server starts listening so a malformed From
	// address or an impossible TLS mode is a startup failure with the variable
	// named, not one FAILED notification per send describing the same mistake
	// in the vocabulary of a delivery error.
	var emailProvider deliver.Provider
	switch cfg.Email.Provider {
	case "":
		log.Warn("no email provider configured — EMAIL notifications will be recorded as FAILED",
			zap.String("set", "NOTIFICATION_EMAIL_PROVIDER=smtp with SMTP_HOST and NOTIFICATION_EMAIL_FROM"))
	case "smtp":
		p, err := deliver.NewSMTPProvider(deliver.SMTPConfig{
			Host:     cfg.Email.Host,
			Port:     cfg.Email.Port,
			Username: cfg.Email.Username,
			Password: cfg.Email.Password,
			From:           cfg.Email.From,
			TLSMode:        deliver.TLSMode(cfg.Email.TLSMode),
			AllowCleartext: cfg.Email.AllowCleartext,
		})
		if err != nil {
			log.Fatal("email provider configuration is invalid", zap.Error(err))
		}
		emailProvider = p
		log.Info("smtp email provider configured",
			zap.String("host", cfg.Email.Host),
			zap.Int("port", cfg.Email.Port),
			zap.String("tls_mode", cfg.Email.TLSMode),
			zap.String("from", cfg.Email.From),
			zap.Bool("authenticated", cfg.Email.Username != ""))

		// Prove the credentials at startup rather than on somebody's password
		// reset.
		//
		// Every fault this catches — unreachable relay, no STARTTLS on the
		// configured port, a rejected password — is permanent and identical
		// for every message, so discovering it on the first real notification
		// means a person waited for an email that was never going to arrive
		// while the log said only "delivery failed".
		//
		// A WARNING, not a Fatal. Refusing to start would take IN_APP
		// notifications down with the mail relay, and IN_APP does not touch
		// SMTP at all — that is precisely the coupling ZS-SVC-Y-001 §9.7 says
		// must not exist. The service runs; EMAIL records FAILED with the
		// provider's own reason until the credential is corrected.
		//
		// Skipped when SMTP_VERIFY_ON_START=false, for an estate where the
		// relay is legitimately not reachable from where the service starts.
		if cfg.Email.VerifyOnStart {
			verifyCtx, cancelVerify := context.WithTimeout(context.Background(), 15*time.Second)
			if err := p.Verify(verifyCtx); err != nil {
				log.Warn("smtp credentials did not verify — EMAIL notifications will fail until this is fixed",
					zap.String("relay", p.Describe()),
					zap.Error(err))
			} else {
				log.Info("smtp credentials verified", zap.String("relay", p.Describe()))
			}
			cancelVerify()
		}
	default:
		log.Fatal("unknown NOTIFICATION_EMAIL_PROVIDER",
			zap.String("provider", cfg.Email.Provider),
			zap.String("supported", "smtp"))
	}

	deliverer := deliver.NewRouter(emailProvider, log)

	// â”€â”€ 4b. Retry policy and worker â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	//
	// One policy, shared. The handler writes the first schedule when a send
	// fails transiently and the worker extends it from there, so they cannot
	// disagree about how many attempts a notification gets.
	retryPolicy := retry.Policy{
		MaxAttempts: cfg.Retry.MaxAttempts,
		BaseDelay:   cfg.Retry.BaseDelay,
		MaxDelay:    cfg.Retry.MaxDelay,
	}
	if !cfg.Retry.Enabled {
		// Not zero: MaxAttempts 1 means "the synchronous attempt and no more",
		// which the handler reports on the record. Zero would Normalize back
		// to the default and quietly re-enable what an operator turned off.
		retryPolicy.MaxAttempts = 1
	}
	retryPolicy = retryPolicy.Normalize()

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("notification-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	h := handler.New(handler.Deps{
		Store:       pgStore,
		Publisher:   publisher,
		AuthZ:       authzClient,
		Deliverer:   deliverer,
		Recipient:   identityClient,
		RetryPolicy: retryPolicy,
		Log:         log,
	})
	handler.RegisterRoutes(r, h)

	// ── 6. Health probes + metrics ────────────────────────────────────────────
	// â”€â”€ 6a. Delivery retry worker â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€
	//
	// Started even when retry is disabled, deliberately. Turning the policy off
	// stops new schedules being written; it does not un-schedule the
	// notifications already waiting, and without a worker those sit PENDING
	// forever with nothing to move them. The worker drains that backlog and
	// then finds nothing, which is the correct behaviour for "disabled".
	workerCtx, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()

	retryWorker := retry.NewWorker(
		pgStore, deliverer, publisher, identityClient, identity.IsSettled,
		retry.Options{
			Interval:  cfg.Retry.Interval,
			BatchSize: cfg.Retry.BatchSize,
			Policy:    retryPolicy,
		}, log)
	go retryWorker.Start(workerCtx)

	healthH := health.New(pool, log)
	r.Get("/healthz", healthH.Liveness)
	r.Get("/readyz", metrics.WrapReadiness(healthH.Readiness))
	r.Handle("/metrics", metrics.MetricsHandler(healthH.Readiness, promhttp.Handler()))

	// ── 7. HTTP server with graceful shutdown ─────────────────────────────────
	addr := ":" + strconv.Itoa(cfg.Port)
	// ReadHeaderTimeout is the one that is easy to miss, and the reason all four
	// are stated together. ReadTimeout bounds a whole request, so a client that
	// dribbles a BODY is already cut off -- but a connection that sends a partial
	// HEADER and then stalls holds a goroutine and a descriptor for that entire
	// window without ever becoming a request. Enough of those exhaust the process
	// while every metric still reads healthy.
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP server listening", zap.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		log.Fatal("server error", zap.Error(err))
	case sig := <-quit:
		log.Info("shutdown signal received", zap.String("signal", sig.String()))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("server stopped")
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Correlation-ID") == "" {
			r.Header.Set("X-Correlation-ID", middleware.GetReqID(r.Context()))
		}
		w.Header().Set("X-Correlation-ID", r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r)
	})
}
