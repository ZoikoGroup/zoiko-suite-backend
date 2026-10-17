package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/bankrec"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
	"zoiko.io/reconciliation-intelligence-svc/internal/financialcontrol"
	"zoiko.io/reconciliation-intelligence-svc/internal/health"
	customMiddleware "zoiko.io/reconciliation-intelligence-svc/internal/middleware"
	"zoiko.io/reconciliation-intelligence-svc/internal/store"
)

const (
	RECONCILIATION_READ             = "RECONCILIATION_READ"
	RECONCILIATION_ANALYZE          = "RECONCILIATION_ANALYZE"
	RECONCILIATION_APPLY_RESOLUTION = "RECONCILIATION_APPLY_RESOLUTION"
	RECONCILIATION_ARCHIVE          = "RECONCILIATION_ARCHIVE"
)

// maxBodyBytes bounds a request body. Every route here takes a small,
// fixed-shape document.
const maxBodyBytes = 64 << 10

type Handler struct {
	store            store.Store
	authz            *authz.Client
	financialControl *financialcontrol.Client
	bankRec          *bankrec.Client
	logger           *zap.Logger
}

func NewHandler(s store.Store, a *authz.Client, fc *financialcontrol.Client, br *bankrec.Client, l *zap.Logger) *Handler {
	return &Handler{
		store:            s,
		authz:            a,
		financialControl: fc,
		bankRec:          br,
		logger:           l,
	}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Canonical Service Input Contract (ZS-ARCH-SVC-001 v2.0 §4). Runs after
	// Recoverer so a refusal is still traced, and ahead of every handler so no
	// request reaches business logic without a resolved tenant, actor,
	// correlation and — on material writes — an idempotency key.
	// Enforcement mode: ZS_ENVELOPE_ENFORCEMENT (default write-strict).
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	// /healthz stays OUTSIDE the tenant gate. Liveness and readiness
	// probes carry no tenant, so a blanket tenant requirement would 401
	// every probe and the orchestrator would restart the container in a
	// loop — a security fix that takes the service down.
	r.Get("/healthz", health.Handler())

	// Everything below requires a gateway-verified tenant. Applied via
	// With() on the route subtree rather than as a path exemption inside
	// the middleware: comparing r.URL.Path to skip an auth gate is a
	// classic bypass source (traversal, trailing slash, case). The route
	// tree is the source of truth for what is and is not protected.
	r.With(customMiddleware.TenantMiddleware).Route("/v1/reconciliations", func(r chi.Router) {
		r.Post("/analyze", h.AnalyzeReconciliation)
		r.Get("/", h.ListJobs)
		r.Get("/{id}", h.GetJobByID)
		r.Post("/{id}/resolutions/{itemId}/apply", h.ApplyResolution)
		r.Delete("/{id}", h.ArchiveJob)
	})

	return r
}

// AnalyzeReconciliation produces advisory suggestions for the UNMATCHED
// statement lines of one bank-reconciliation run. The lines are read from
// bank-reconciliation-svc by run id; the caller names the run, it does not
// supply transactions. Nothing is written back to bank-reconciliation-svc.
func (h *Handler) AnalyzeReconciliation(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())

	// Unknown fields are refused: a caller still sending the old
	// transactions_a / transactions_b body must be told those are no longer
	// read, not have them silently dropped.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req domain.AnalyzeReconciliationRequest
	if err := dec.Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if err := req.Validate(); err != nil {
		h.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), tenantID, principalID, req.LegalEntityID, RECONCILIATION_ANALYZE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	scope := bankrec.Scope{TenantID: tenantID, PrincipalID: principalID, LegalEntityID: req.LegalEntityID}
	run, err := h.bankRec.GetRun(r.Context(), scope, req.BankReconciliationRunID)
	if err != nil {
		h.writeBankRecErr(w, err)
		return
	}
	// The caller was authorized against req.LegalEntityID; the run must be
	// that entity's, or the authorization and the data would be about
	// different entities.
	if run.LegalEntityID != req.LegalEntityID {
		h.respondError(w, http.StatusForbidden, "bank reconciliation run does not belong to legal_entity_id")
		return
	}
	if run.Superseded() {
		msg := "bank reconciliation run is superseded"
		if run.SupersededByRunID != nil {
			msg += " by run " + *run.SupersededByRunID
		}
		h.respondError(w, http.StatusConflict, msg)
		return
	}

	// ZS-SVC-Z-001 INV-09: tolerance must be explicit and versioned. The
	// governed tolerance is fetched from financial-control-svc before any
	// recommendation is produced — an unreachable service or a legal entity
	// with no policy configured yet refuses the whole analyze call rather
	// than falling back to a guessed default.
	missingReferenceTolerance, err := h.financialControl.GetActiveAbsoluteTolerance(
		r.Context(), tenantID, principalID, req.LegalEntityID, domain.MetricMissingReferenceTolerance)
	if err != nil {
		h.writeToleranceErr(w, err, domain.MetricMissingReferenceTolerance)
		return
	}

	lines, err := h.bankRec.ListUnmatchedLines(r.Context(), scope, run)
	if err != nil {
		h.writeBankRecErr(w, err)
		return
	}

	items, err := domain.AnalyzeUnmatchedStatementLines(lines, run.RunID, tenantID, missingReferenceTolerance)
	if err != nil {
		h.logger.Error("analysis of bank reconciliation lines failed", zap.String("run_id", run.RunID), zap.Error(err))
		h.respondError(w, http.StatusBadGateway, "bank-reconciliation-svc returned lines that could not be analyzed")
		return
	}

	job := &domain.ReconciliationJob{
		LegalEntityID:           req.LegalEntityID,
		JobName:                 req.JobName,
		BankReconciliationRunID: run.RunID,
		SourceSystemA:           domain.SourceBankStatement,
		TotalProcessedCount:     len(lines),
		UnmatchedCount:          len(items),
		Status:                  "COMPLETED",
	}

	if err := h.store.CreateJob(r.Context(), tenantID, job, items, principalID, r.Header.Get("X-Correlation-ID")); err != nil {
		h.logger.Error("failed to save reconciliation job", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to run reconciliation analysis")
		return
	}

	h.respondJSON(w, http.StatusCreated, job)
}

// ListJobs lists one legal entity's jobs. legal_entity_id is required: a
// read is authorized per entity, and "every entity in the tenant" is not
// something a single authorization decision can grant.
func (h *Handler) ListJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	sourceA := r.URL.Query().Get("source_system_a")
	status := r.URL.Query().Get("status")

	if legalEntityID == "" {
		h.respondError(w, http.StatusBadRequest, "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), tenantID, principalID, legalEntityID, RECONCILIATION_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	jobs, err := h.store.ListJobs(r.Context(), tenantID, legalEntityID, sourceA, status)
	if err != nil {
		h.logger.Error("failed to list reconciliation jobs", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to query reconciliation jobs")
		return
	}

	if jobs == nil {
		jobs = []domain.ReconciliationJob{}
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"data":  jobs,
		"count": len(jobs),
	})
}

// GetJobByID is authorized against the job's own legal entity, in the same
// order the write routes use: load (tenant-scoped by RLS), then check.
func (h *Handler) GetJobByID(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "reconciliation job not found")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), tenantID, principalID, job.LegalEntityID, RECONCILIATION_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	h.respondJSON(w, http.StatusOK, job)
}

func (h *Handler) ApplyResolution(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	jobID := chi.URLParam(r, "id")
	itemID := chi.URLParam(r, "itemId")

	var req domain.ApplyResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.ResolutionStatus == "" {
		req.ResolutionStatus = domain.StatusApproved
	}
	if err := domain.ValidateResolutionTarget(req.ResolutionStatus); err != nil {
		h.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "reconciliation job not found")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), tenantID, principalID, job.LegalEntityID, RECONCILIATION_APPLY_RESOLUTION); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	item, err := h.store.ApplyResolution(r.Context(), tenantID, jobID, itemID, req.ResolutionStatus, req.ResolutionNotes,
		job.LegalEntityID, principalID, r.Header.Get("X-Correlation-ID"))
	if errors.Is(err, domain.ErrResolutionTransition) {
		h.respondError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		h.respondError(w, http.StatusNotFound, err.Error())
		return
	}

	h.respondJSON(w, http.StatusOK, item)
}

func (h *Handler) ArchiveJob(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "reconciliation job not found")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), tenantID, principalID, job.LegalEntityID, RECONCILIATION_ARCHIVE); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.ArchiveJob(r.Context(), tenantID, id); err != nil {
		h.respondError(w, http.StatusNotFound, "reconciliation job not found")
		return
	}

	h.respondJSON(w, http.StatusOK, map[string]string{
		"message": "reconciliation job archived successfully",
		"id":      id,
	})
}

func (h *Handler) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) respondError(w http.ResponseWriter, code int, message string) {
	h.respondJSON(w, code, map[string]string{"error": message})
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.respondError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr maps an authz.CheckAllowed error to the appropriate HTTP
// response. Denial is 403; any other error (including authorization-svc
// being unreachable) is 503 — fail closed, never allow silently.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		h.respondError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	h.respondError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// writeBankRecErr maps a bank-reconciliation-svc read failure. Every case
// refuses the analysis — no suggestion is ever produced from a population
// that could not be read completely and exactly.
func (h *Handler) writeBankRecErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bankrec.ErrRunNotFound):
		h.respondError(w, http.StatusNotFound, "bank reconciliation run not found")
	case errors.Is(err, bankrec.ErrPopulationTooLarge):
		h.respondError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, bankrec.ErrServiceUnavailable):
		h.logger.Error("bank-reconciliation-svc read failed", zap.Error(err))
		h.respondError(w, http.StatusServiceUnavailable, "bank-reconciliation-svc unavailable")
	default:
		// ErrRequestRefused, ErrInconsistent, ErrScopeMissing: the upstream
		// answered, but not with something this service can stand behind.
		h.logger.Error("bank-reconciliation-svc read failed", zap.Error(err))
		h.respondError(w, http.StatusBadGateway, "bank-reconciliation-svc returned an unusable response")
	}
}

// writeToleranceErr responds to a failure resolving a governed tolerance
// policy (ZS-SVC-Z-001 INV-09). ErrToleranceNotConfigured is a 424
// (Failed Dependency) — the request itself was fine, but the legal
// entity has no governed policy for this metric yet, which is a
// configuration gap for an operator to close, not a transient outage.
// Any other failure is treated as financial-control-svc being
// unreachable — 503, fail closed, never a guessed default.
func (h *Handler) writeToleranceErr(w http.ResponseWriter, err error, metric string) {
	if errors.Is(err, financialcontrol.ErrToleranceNotConfigured) {
		h.respondError(w, http.StatusFailedDependency, "no governed tolerance policy configured for metric "+metric)
		return
	}
	h.logger.Error("financial-control-svc tolerance lookup failed", zap.String("metric", metric), zap.Error(err))
	h.respondError(w, http.StatusServiceUnavailable, "financial-control-svc unavailable")
}
