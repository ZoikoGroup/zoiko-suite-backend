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

	"zoiko.io/accounts-receivable-svc/internal/ar01"
	"zoiko.io/accounts-receivable-svc/internal/ar02"
	"zoiko.io/accounts-receivable-svc/internal/ar03"
	"zoiko.io/accounts-receivable-svc/internal/ar04"
	"zoiko.io/accounts-receivable-svc/internal/ar05"
	"zoiko.io/accounts-receivable-svc/internal/ar06"
	"zoiko.io/accounts-receivable-svc/internal/ar07"
	"zoiko.io/accounts-receivable-svc/internal/ar08"
	"zoiko.io/accounts-receivable-svc/internal/ar09"
	"zoiko.io/accounts-receivable-svc/internal/ar10"
	"zoiko.io/accounts-receivable-svc/internal/arctl"
	"zoiko.io/accounts-receivable-svc/internal/authz"
	"zoiko.io/accounts-receivable-svc/internal/config"
	"zoiko.io/accounts-receivable-svc/internal/entity"
	svcenvelope "zoiko.io/accounts-receivable-svc/internal/envelope"
	"zoiko.io/accounts-receivable-svc/internal/events"
	"zoiko.io/accounts-receivable-svc/internal/handler"
	"zoiko.io/accounts-receivable-svc/internal/health"
	"zoiko.io/accounts-receivable-svc/internal/kit"
	"zoiko.io/accounts-receivable-svc/internal/ledger"
	svcmiddleware "zoiko.io/accounts-receivable-svc/internal/middleware"
	"zoiko.io/accounts-receivable-svc/internal/mtls"
	"zoiko.io/accounts-receivable-svc/internal/outbox"
	"zoiko.io/accounts-receivable-svc/internal/store"
	"zoiko.io/accounts-receivable-svc/internal/taxdet"
	"zoiko.io/accounts-receivable-svc/internal/telemetry"
)

// platformScopeID mirrors authorization-svc's own constant of the same
// name — this service's mTLS identity is infrastructure, not tenant data.
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

	log.Info("accounts-receivable-svc starting",
		zap.Int("port", cfg.Port),
		zap.String("db_host", cfg.DB.Host),
		zap.String("authz_url", cfg.AuthZServiceURL),
		zap.String("ledger_url", cfg.LedgerServiceURL),
		zap.String("tenant_registry_url", cfg.TenantRegistryURL),
	)

	// ── 2b. Tracing ──────────────────────────────────────────────────────────
	shutdownTracing, err := telemetry.InitTracing(context.Background(), "accounts-receivable-svc", cfg.OTELExporterEndpoint)
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

	metrics := telemetry.NewMetrics("accounts-receivable-svc")

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

	// Verify connectivity at startup
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pingCancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Fatal("db unreachable at startup", zap.Error(err))
	}
	log.Info("db pool connected")

	// ── 4. Store, Kafka producer, clients ─────────────────────────────────────
	pgStore := store.New(pool, log)

	// AllowAutoTopicCreation is required even though the broker itself has
	// auto.create.topics.enable=true: segmentio/kafka-go's Writer defaults
	// this to false and never asks the broker to auto-create in its
	// metadata request, so every write to a not-yet-existing topic fails
	// with "Unknown Topic Or Partition" regardless of the broker-side
	// setting.
	kafkaWriter := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Kafka.Brokers...),
		Topic:                  cfg.Kafka.Topic,
		Balancer:               &kafka.LeastBytes{},
		AllowAutoTopicCreation: true,
		// The library default is one second, and these publishes are
		// synchronous: every invoice issue, send, overdue and payment paid a
		// full second waiting for a batch that was never going to fill, on top
		// of the authorization round trip. Matches the rest of the platform.
		BatchTimeout: 10 * time.Millisecond,
	}
	defer func() { _ = kafkaWriter.Close() }()

	publisher := events.NewPublisher(log, cfg.Kafka.Topic, kafkaWriter)

	// ── 4b. Transactional Outbox Relay (ZS-STATE-001 Invariant I-13) ──────────
	relayCtx, cancelRelay := context.WithCancel(context.Background())
	defer cancelRelay()
	relay := outbox.NewRelay(pool, publisher, 500*time.Millisecond, 50, log)
	go relay.Start(relayCtx)

	ledgerClient := ledger.NewHTTPClient(cfg.LedgerServiceURL, log)
	entityClient := entity.NewHTTPClient(cfg.TenantRegistryURL)

	var authzClient *authz.HTTPClient
	if cfg.AuthzMTLSEnabled {
		mtlsHTTPClient, err := mtls.NewClientHTTPClient(context.Background(), cfg.MTLSManagementServiceURL, "accounts-receivable-svc", platformScopeID)
		if err != nil {
			log.Fatal("mtls: failed to provision client identity", zap.Error(err))
		}
		log.Info("mTLS enabled for authorization-svc calls", zap.String("authz_mtls_url", cfg.AuthzMTLSURL))
		authzClient = authz.NewClientWithHTTPClient(cfg.AuthzMTLSURL, log, mtlsHTTPClient)
	} else {
		authzClient = authz.NewHTTPClient(cfg.AuthZServiceURL, log)
	}

	// ── 5. Router + handler ───────────────────────────────────────────────────
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(otelchi.Middleware("accounts-receivable-svc", otelchi.WithChiRoutes(r)))
	r.Use(metrics.HTTPMiddleware)
	r.Use(correlationIDMiddleware)
	r.Use(svcmiddleware.TenantContext())
	r.Use(middleware.Logger)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer and telemetry so a refusal is still traced, and ahead of every
	// handler so no request reaches business logic without a resolved tenant,
	// actor, correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	//
	// The AR-05 provider callback is exempt: an external e-invoice provider has no
	// tenant, principal or idempotency key to send. It is authenticated by its
	// HMAC signature instead (see internal/ar05 callback.go). The gateway must
	// route this exact path around ForwardAuth, ideally IP-allowlisted.
	envPolicy := svcenvelope.ServicePolicy()
	envPolicy.ExemptPaths = append(envPolicy.ExemptPaths, ar05.CallbackPath)
	r.Use(svcenvelope.Middleware(envPolicy, svcenvelope.DefaultReporter()))

	h := handler.New(pgStore, publisher, authzClient, ledgerClient, entityClient, log)
	handler.RegisterRoutes(r, h)

	// AR-01 Customer Financial Profile (ZS-SVC-C-001). getCreditStatus needs
	// AR-07 open items for exposure; until that module is wired it reports
	// exposure_available:false rather than guessing.
	kitDeps := kit.Deps{DB: kit.NewDB(pool), Log: log, Authz: authzClient, Entities: entityClient}
	// AR-07 Receivables is built first: AR-01 (credit exposure), AR-03 (credit
	// check) and AR-04 (receivable creation at issue) consume it as kit.OpenItems.
	// Its invoice-dimension mirror is late-bound to AR-04 below (construction cycle).
	ar07m := ar07.New(kitDeps)
	ar07m.Register(r)

	ar01m := ar01.New(kitDeps, ar01.NewCounterpartyDirectory(cfg.CounterpartyMgmtURL), ar07m)
	ar01m.Register(r)

	// AR-03 Sales Order. kit.OpenItems is AR-07's; until it is wired the credit
	// check compares only the order's own value and records exposure_available:false.
	// (Pass a literal nil: a typed-nil module would count as non-nil.)
	ar03m := ar03.New(kitDeps, ar01m, ar07m)
	ar03m.Register(r)

	// AR-02 Quote / Estimate. AR-03 is the order creator, so quote conversion
	// and order creation commit atomically.
	ar02m := ar02.New(kitDeps, ar01m, taxdet.NewHTTPClient(cfg.TaxDeterminationURL), ar03m)
	ar02m.Register(r)

	// AR-04 Customer Invoice. Receivable creation needs AR-07 (kit.OpenItems,
	// literal nil until it exists): until then issueInvoice answers 503 and
	// changes nothing, because an issued invoice with no receivable is a control
	// defect. e-invoice requirement (AR-05) nil = NOT_REQUIRED.
	// AR-04 takes the AR-05 requirement and AR-05 takes AR-04's kit.Invoices; the
	// forwarding proxy (late-bound below) breaks the construction cycle without
	// handing AR-04 a typed-nil pointer.
	einvoiceReq := &einvoiceRequirementProxy{}
	ar04m := ar04.New(kitDeps, ar01m, ar03m, ar07m,
		taxdet.NewHTTPClient(cfg.TaxDeterminationURL),
		ar04.NewHTTPPeriodGate(cfg.AccountingPeriodURL), einvoiceReq)
	ar04m.Register(r)
	ar07m.SetInvoices(ar04m) // settlement / dispute state mirrored onto the invoice dimension

	// AR-06 Credit / Debit Note. Built before AR-05 because AR-05 takes AR-06 as
	// the document source for CREDIT_NOTE / DEBIT_NOTE; it reaches AR-05's
	// requirement through the same late-bound proxy as AR-04.
	ar06m := ar06.New(kitDeps, ar04m, ar07m,
		taxdet.NewHTTPClient(cfg.TaxDeterminationURL),
		ar04.NewHTTPPeriodGate(cfg.AccountingPeriodURL), einvoiceReq)
	ar06m.Register(r)

	// AR-08 Cash Application. Settlement authority is Banking: a receipt exists
	// only by registering a banking-connector-svc canonical transaction that is
	// NORMALIZED (confirmed). AR-07 serves both as the open-item port and as the
	// candidate finder; the legacy manual /v1/invoices/{id}/pay stays refused on
	// new-path invoices.
	ar08m := ar08.New(kitDeps, ar08.NewHTTPReceiptSource(cfg.BankingConnectorURL), ar07m, ar07m,
		ar04.NewHTTPPeriodGate(cfg.AccountingPeriodURL))
	ar08m.Register(r)

	// AR-10 Refund / Write-Off. Refunds draw on unapplied cash reserved on an
	// AR-08 receipt and settle ONLY when payment-status-svc reports SETTLED (AR
	// never marks settlement itself). Write-off authority is a constant-tier
	// policy until delegated-authority-svc offers a limit-check API.
	ar10m := ar10.New(kitDeps, ar07m, ar04.NewHTTPPeriodGate(cfg.AccountingPeriodURL), ar08m,
		ar10.NewHTTPGateway(cfg.PaymentInitiationURL, cfg.PaymentStatusURL),
		ar10.NewHTTPDestinationVerifier(cfg.PayeeBankingURL), ar07m, nil)
	ar10m.Register(r)

	// AR-to-GL controls (spec invariants 15/17, §17). financial-control-svc pulls
	// `open-invoices` (AR subledger to GL, FIN-CTRL-001; sequence gap, FIN-CTRL-027):
	// it now serves legacy + new-path invoices together. The reconciliation module
	// checks population completeness (not just totals), accounting-event completeness
	// and an exact subledger-to-GL tie-out; an unreadable GL is UNVERIFIED, never a
	// pass, and AR owns no tolerance (financial-control-svc's policy).
	arPG := arctl.PgSources{DB: kitDeps.DB}
	invoicesUnion := arctl.Union(pgStore.ControlPopulation, arctl.NewPathInvoices(kitDeps.DB))
	for _, n := range []string{"open-invoices", "invoices"} {
		handler.RegisterPopulation(n, invoicesUnion)
	}
	for _, n := range []string{"open-items", "open_items"} {
		handler.RegisterPopulation(n, ar07m.ControlPopulation)
	}
	for _, n := range []string{"credit-debit-notes", "credit_debit_notes"} {
		handler.RegisterPopulation(n, arctl.CreditDebitNotes(kitDeps.DB))
	}
	for _, n := range []string{"cash-receipts", "cash_receipts"} {
		handler.RegisterPopulation(n, arctl.CashReceipts(kitDeps.DB))
	}
	ar10Exceptions := arctl.NewExceptionSource("AR10", func(ctx context.Context, tenantID, legalEntityID string) ([]arctl.ExceptionItem, error) {
		ex, err := ar10m.ControlExceptions(ctx, tenantID, legalEntityID)
		if err != nil {
			return nil, err
		}
		out := make([]arctl.ExceptionItem, 0, len(ex))
		for _, e := range ex {
			out = append(out, arctl.ExceptionItem{Source: "AR10", Kind: e.Kind, AggregateID: e.AggregateID, Detail: e.Detail})
		}
		return out, nil
	})
	arctlm := arctl.New(kitDeps, arctl.Sources{
		Documents:    arPG,
		Completeness: ar07m,
		Postings:     arPG,
		Subledger:    arctl.PopulationSubledger{Provider: invoicesUnion},
		GL:           arctl.NewHTTPGLBalance(cfg.LedgerServiceURL),
		Exceptions:   append(arPG.Rollup(time.Now, arctl.DefaultPEPendingEInvoice), ar10Exceptions),
	}).WithGLPrincipal(cfg.PostingPrincipalID)
	arctlm.Register(r)

	// AR-09 Collections & Dunning. Collections never change a balance: AR-07
	// facts are read live. No notification adapter can address an external
	// customer yet (notification-svc ingest needs a platform principal), so the
	// deliverer is nil = UnconfiguredDeliverer, which records DELIVERY_FAILED and
	// never fakes success. The scheduler is OFF unless AR09_SCHEDULER_ENABLED=true;
	// it scans all tenants, so it needs a DB role that bypasses RLS (like the outbox).
	ar09m := ar09.New(kitDeps, ar07m, ar01m, nil)
	ar09m.Register(r)
	if os.Getenv("AR09_SCHEDULER_ENABLED") == "true" {
		interval := time.Hour
		if d, err := time.ParseDuration(os.Getenv("AR09_SCHEDULER_INTERVAL")); err == nil && d > 0 {
			interval = d
		}
		go ar09m.Start(relayCtx, interval)
	}

	// AR-05 Electronic Invoice. No transport exists in the platform yet:
	// Transports nil = UnconfiguredTransports (submit answers 503
	// EINVOICE_PROVIDER_NOT_CONFIGURED, never a fake success). Which profile a
	// jurisdiction uses is deployment configuration, e.g.
	// AR_EINVOICE_PROFILES="GB|INVOICE=PEPPOL_INVOICE,DE|INVOICE=XRECHNUNG".
	// Provider callback secrets: AR_EINVOICE_CALLBACK_SECRET_<PROVIDER>.
	eiCatalog, err := ar05.ParseProfileCatalog(os.Getenv("AR_EINVOICE_PROFILES"))
	if err != nil {
		log.Fatal("AR_EINVOICE_PROFILES is invalid", zap.Error(err))
	}
	ar05m := ar05.New(kitDeps, ar05.Collaborators{
		Invoices: ar04m,
		Profiles: ar05.NewHTTPProfileResolver(cfg.JurisdictionRulesURL),
		Catalog:  eiCatalog,
		Entities: ar05.NewHTTPEntityJurisdictions(cfg.TenantRegistryURL, cfg.JurisdictionRulesURL),
		Registry: ar05.NewHTTPRegistry(cfg.JurisdictionRulesURL),
		Sources: map[string]ar05.DocumentSource{
			kit.DocCreditNote: ar06m.EInvoiceSource(ar06.KindCredit),
			kit.DocDebitNote:  ar06m.EInvoiceSource(ar06.KindDebit),
		},
	})
	ar05m.Register(r)
	einvoiceReq.m = ar05m

	// Durable ACC-04 posting queue (spec invariant 14: AR never writes the GL
	// directly). Each module that owns a posting source registers its listener.
	postingDispatcher := kit.NewPostingDispatcher(kitDeps.DB, kit.NewHTTPLedgerPoster(cfg.LedgerServiceURL), cfg.PostingPrincipalID, log)
	postingDispatcher.Register(kit.SourceInvoice, ar04m)
	postingDispatcher.Register(kit.SourceCreditNote, ar06m)
	postingDispatcher.Register(kit.SourceDebitNote, ar06m)
	postingDispatcher.Register(kit.SourceCashApplication, ar08m)
	postingDispatcher.Register(kit.SourceCashUnapply, ar08m)
	postingDispatcher.Register(kit.SourceRefund, ar10m)
	postingDispatcher.Register(kit.SourceWriteOff, ar10m)
	go postingDispatcher.Start(relayCtx)

	// Legacy /v1/invoices create and pay are deprecated in favour of AR-04 / AR-08.
	// They stay on by default; AR_LEGACY_INVOICE_API=false retires them (410), and
	// pay on any new-path invoice is always refused (CASH_APPLICATION_REQUIRED).
	h.WithLegacyInvoiceAPI(os.Getenv("AR_LEGACY_INVOICE_API") != "false").WithNewPathGuard(ar04m)

	// ── 6. Health probes + metrics ────────────────────────────────────────────
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
		cancelRelay()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", zap.Error(err))
	}
	log.Info("server stopped")
}

// einvoiceRequirementProxy forwards kit.EInvoiceRequirement to AR-05 once it is
// built; until then an e-invoice is not required.
type einvoiceRequirementProxy struct{ m kit.EInvoiceRequirement }

func (p *einvoiceRequirementProxy) Required(ctx context.Context, tenantID, legalEntityID, jurisdictionID, documentType string) (bool, error) {
	if p.m == nil {
		return false, nil
	}
	return p.m.Required(ctx, tenantID, legalEntityID, jurisdictionID, documentType)
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
