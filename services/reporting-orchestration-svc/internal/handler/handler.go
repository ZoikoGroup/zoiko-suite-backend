// Package handler is reporting-orchestration-svc's HTTP surface.
//
// Code path for every request (see ROUTES.md for the full table):
//
//	router (this file)
//	  -> chi RequestID / RealIP / Recoverer
//	  -> middleware.TenantMiddleware (X-Tenant-Id required, 401; probes exempt)
//	  -> envelope.Middleware        (canonical input contract, 401/400; probes exempt)
//	  -> idempotencyMiddleware      (Idempotency-Key replay, writes only)
//	  -> handler method             (validation -> authz -> store)
//	  -> store.PgStore              (tenant-scoped tx, state + outbox event)
package handler

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/reporting-orchestration-svc/internal/archivestore"
	"zoiko.io/reporting-orchestration-svc/internal/domain"
	svcenvelope "zoiko.io/reporting-orchestration-svc/internal/envelope"
	"zoiko.io/reporting-orchestration-svc/internal/health"
	"zoiko.io/reporting-orchestration-svc/internal/middleware"
	"zoiko.io/reporting-orchestration-svc/internal/retention"
	"zoiko.io/reporting-orchestration-svc/internal/store"
)

// Authorization action types sent to authorization-svc.
const (
	actionDefinitionCreate       = "REPORT_DEFINITION_CREATE"
	actionDefinitionStatusUpdate = "REPORT_DEFINITION_STATUS_UPDATE"
	actionRunTrigger             = "REPORT_RUN_TRIGGER"
	actionExportRequest          = "AUDIT_EXPORT_REQUEST"
	actionExportApprove          = "AUDIT_EXPORT_APPROVE"
	actionExportBuild            = "AUDIT_EXPORT_BUILD"
	actionExportRedact           = "AUDIT_EXPORT_REDACT"
	actionExportSeal             = "AUDIT_EXPORT_SEAL"
	actionExportDeliver          = "AUDIT_EXPORT_DELIVER"
	actionExportReceive          = "AUDIT_EXPORT_RECEIVE" // checked for the recipient
	actionExportRevoke           = "AUDIT_EXPORT_REVOKE"
)

// Authorizer asks authorization-svc whether a principal may act (authz.Client).
type Authorizer interface {
	CheckAllowed(ctx context.Context, tenantID, principalID, legalEntityID, actionType string) error
}

// DefinitionStore persists report definitions.
type DefinitionStore interface {
	CreateDefinition(ctx context.Context, definition *domain.ReportDefinition, event domain.OutboxEvent) (*domain.ReportDefinition, error)
	GetDefinition(ctx context.Context, tenantID, id string) (*domain.ReportDefinition, error)
	ListDefinitions(ctx context.Context, tenantID string, filter store.DefinitionFilter, page store.Page) ([]domain.ReportDefinition, error)
	TransitionDefinitionStatus(ctx context.Context, p store.TransitionDefinitionParams) (*store.DefinitionTransition, error)
}

// RunStore persists report runs.
type RunStore interface {
	CreateRun(ctx context.Context, run *domain.ReportRun, event domain.OutboxEvent) (*domain.ReportRun, error)
	GetRun(ctx context.Context, tenantID, id string) (*domain.ReportRun, error)
	ListRuns(ctx context.Context, tenantID string, filter store.RunFilter, page store.Page) ([]domain.ReportRun, error)
}

// ExportStore persists AUD-10 audit exports and their evidence rows.
type ExportStore interface {
	CreateExportRequest(ctx context.Context, p domain.CreateExportRequestParams) (*domain.AuditExportRequest, error)
	GetExportRequest(ctx context.Context, tenantID, exportID string) (*domain.AuditExportRequest, error)
	ListExportRequests(ctx context.Context, tenantID string, filter store.ExportFilter, page store.Page) ([]domain.AuditExportRequest, error)
	ApproveExport(ctx context.Context, p domain.ApproveExportParams) (*domain.AuditExportRequest, error)
	StartBuild(ctx context.Context, p domain.StartBuildParams) (*domain.AuditExportRequest, error)
	CompleteBuild(ctx context.Context, p domain.CompleteBuildParams) (*domain.AuditExportRequest, error)
	FailBuild(ctx context.Context, p domain.FailBuildParams) (*domain.AuditExportRequest, error)
	RecordRedaction(ctx context.Context, p domain.RecordRedactionParams) (*domain.RedactionDecision, error)
	SealExport(ctx context.Context, p domain.SealExportParams) (*domain.AuditExportRequest, error)
	DeliverExport(ctx context.Context, p domain.DeliverExportParams) (*domain.AuditExportRequest, error)
	RevokeExport(ctx context.Context, p domain.RevokeExportParams) (*domain.AuditExportRequest, error)
	GetPackageManifest(ctx context.Context, tenantID, exportID string) (*domain.AuditExportRequest, []domain.ExportManifest, []domain.RedactionDecision, error)
	ListRedactions(ctx context.Context, tenantID, exportID string) ([]domain.RedactionDecision, error)
	ListDeliveries(ctx context.Context, tenantID, exportID string) ([]domain.DeliveryReceipt, error)
}

// Dependencies are everything the handler needs; main.go wires them.
type Dependencies struct {
	Definitions DefinitionStore
	Runs        RunStore
	Exports     ExportStore
	Idempotency IdempotencyStore
	Archives    archivestore.Client
	Retentions  retention.Client
	Authz       Authorizer
	Ready       health.Pinger // decides /readyz (the Postgres pool)
	Logger      *zap.Logger
}

type Handler struct {
	definitions DefinitionStore
	runs        RunStore
	exports     ExportStore
	idempotency IdempotencyStore
	archives    archivestore.Client
	retentions  retention.Client
	authz       Authorizer
	ready       health.Pinger
	logger      *zap.Logger
}

func NewHandler(deps Dependencies) *Handler {
	logger := deps.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Handler{
		definitions: deps.Definitions,
		runs:        deps.Runs,
		exports:     deps.Exports,
		idempotency: deps.Idempotency,
		archives:    deps.Archives,
		retentions:  deps.Retentions,
		authz:       deps.Authz,
		ready:       deps.Ready,
		logger:      logger,
	}
}

// NewRouter builds the full route tree.
func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Use(chiMiddleware.RequestID)
	r.Use(chiMiddleware.RealIP)
	r.Use(chiMiddleware.Recoverer)
	r.Use(middleware.TenantMiddleware)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4): tenant,
	// actor, request id, correlation id, source channel, legal entity on
	// writes, and Idempotency-Key on writes. Mode: ZS_ENVELOPE_ENFORCEMENT.
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))
	// Every write passes through Idempotency-Key replay protection.
	r.Use(h.idempotencyMiddleware)

	// Probes: exempted by the tenant gate and the envelope policy.
	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(h.ready))

	r.Route("/v1/reports", func(r chi.Router) {
		r.Post("/definitions", h.CreateDefinition)
		r.Get("/definitions", h.ListDefinitions)
		r.Get("/definitions/{id}", h.GetDefinition)
		r.Patch("/definitions/{id}/status", h.UpdateDefinitionStatus)
		r.Post("/definitions/{id}/runs", h.TriggerRun)
		r.Get("/runs", h.ListRuns)
		r.Get("/runs/{runId}", h.GetRun)
	})

	r.Route("/v1/audit-exports", func(r chi.Router) {
		r.Post("/", h.CreateExport)
		r.Get("/", h.ListExports)
		r.Get("/{id}", h.GetExport)
		r.Post("/{id}/approve", h.ApproveExport)
		r.Post("/{id}/build", h.BuildExport)
		r.Post("/{id}/redact", h.RedactExport)
		r.Post("/{id}/seal", h.SealExport)
		r.Post("/{id}/deliver", h.DeliverExport)
		r.Post("/{id}/revoke", h.RevokeExport)
		r.Get("/{id}/manifest", h.GetManifest)
		r.Get("/{id}/redactions", h.ListRedactions)
		r.Get("/{id}/deliveries", h.ListDeliveries)
	})

	// Unknown routes and wrong methods get problem+json, not chi's text.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		writeProblem(w, req, http.StatusNotFound, CodeNotFound, "no route matches this path")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		writeProblem(w, req, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "this method is not allowed on this path")
	})
	return r
}
