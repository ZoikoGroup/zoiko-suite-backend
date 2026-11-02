package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"zoiko.io/reconciliation-intelligence-svc/internal/authz"
	"zoiko.io/reconciliation-intelligence-svc/internal/bankrec"
	"zoiko.io/reconciliation-intelligence-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-intelligence-svc/internal/envelope"
	"zoiko.io/reconciliation-intelligence-svc/internal/financialcontrol"
	"zoiko.io/reconciliation-intelligence-svc/internal/health"
	"zoiko.io/reconciliation-intelligence-svc/internal/idempotency"
	"zoiko.io/reconciliation-intelligence-svc/internal/ledger"
	customMiddleware "zoiko.io/reconciliation-intelligence-svc/internal/middleware"
	"zoiko.io/reconciliation-intelligence-svc/internal/problem"
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
	ledger           *ledger.Client
	// searchHorizonDays bounds the candidate search's ledger read; see
	// config.CandidateSearchHorizonDays.
	searchHorizonDays int
	logger            *zap.Logger
}

func NewHandler(s store.Store, a *authz.Client, fc *financialcontrol.Client, br *bankrec.Client, gl *ledger.Client, searchHorizonDays int, l *zap.Logger) *Handler {
	return &Handler{
		store:             s,
		authz:             a,
		financialControl:  fc,
		bankRec:           br,
		ledger:            gl,
		searchHorizonDays: searchHorizonDays,
		logger:            l,
	}
}

// NewRouter builds the route tree with no database behind /readyz (it
// reports 503) and no Idempotency-Key replay store. The server uses
// NewRouterWithDeps.
func NewRouter(h *Handler) http.Handler {
	return NewRouterWithDeps(h, health.ReadyzHandler(nil), nil)
}

// NewRouterWithDeps is NewRouter with the readiness probe and the
// Idempotency-Key replay store supplied (health.ReadyzHandler(pool) and
// idempotency.NewPgStore(pool) in production). A nil idem disables replay.
func NewRouterWithDeps(h *Handler, readyz http.HandlerFunc, idem idempotency.Store) http.Handler {
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

	// Idempotency-Key replay: a retried write with the same key returns the
	// first answer instead of running again. Reads and probes pass through.
	if idem != nil {
		r.Use(idempotency.Middleware(idem, h.logger))
	}

	// /healthz stays OUTSIDE the tenant gate. Liveness and readiness
	// probes carry no tenant, so a blanket tenant requirement would 401
	// every probe and the orchestrator would restart the container in a
	// loop — a security fix that takes the service down.
	r.Get("/healthz", health.Handler())
	r.Get("/readyz", readyz)

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
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			problem.Write(w, r, http.StatusRequestEntityTooLarge, "payload_too_large", fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))
			return
		}
		problem.Write(w, r, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}

	if err := req.Validate(); err != nil {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	// The header is the tenant authority; a body tenant_id may only repeat it.
	if req.TenantID != "" && req.TenantID != tenantID {
		problem.Write(w, r, http.StatusForbidden, "tenant_mismatch", "body tenant_id does not match X-Tenant-Id")
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
		problem.Write(w, r, http.StatusForbidden, "run_entity_mismatch", "bank reconciliation run does not belong to legal_entity_id")
		return
	}
	if run.Superseded() {
		msg := "bank reconciliation run is superseded"
		if run.SupersededByRunID != nil {
			msg += " by run " + *run.SupersededByRunID
		}
		problem.Write(w, r, http.StatusConflict, "run_superseded", msg)
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
	// The candidate search's date tolerance is governed the same way (A2a):
	// AUTO_MATCH versus DATE_SKEW is decided by an approved, versioned
	// policy, never a constant in this service.
	dateTolerance, err := h.financialControl.GetActiveDateTolerance(
		r.Context(), tenantID, principalID, req.LegalEntityID, domain.MetricCandidateDateTolerance)
	if err != nil {
		h.writeToleranceErr(w, err, domain.MetricCandidateDateTolerance)
		return
	}

	lines, err := h.bankRec.ListUnmatchedLines(r.Context(), scope, run)
	if err != nil {
		h.writeBankRecErr(w, err)
		return
	}

	search, err := h.readCandidates(r.Context(), ledger.Scope(scope), lines, dateTolerance)
	if err != nil {
		h.writeLedgerErr(w, err)
		return
	}

	items, err := domain.AnalyzeUnmatchedStatementLines(lines, run.RunID, tenantID, missingReferenceTolerance, search)
	if err != nil {
		h.logger.Error("analysis of bank reconciliation lines failed", zap.String("run_id", run.RunID), zap.Error(err))
		problem.Write(w, r, http.StatusBadGateway, "bank_reconciliation_unusable", "bank-reconciliation-svc returned lines that could not be analyzed")
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
		problem.Write(w, r, http.StatusInternalServerError, "internal_error", "failed to run reconciliation analysis")
		return
	}

	setETag(w, job.Version)
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
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", "legal_entity_id is required")
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

	if status != "" && !domain.KnownJobStatuses[status] {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", "status must be one of ANALYZING, COMPLETED, ARCHIVED")
		return
	}
	if sourceA != "" && !domain.KnownSourceSystems[domain.SourceSystem(sourceA)] {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", "source_system_a is not a known source system")
		return
	}
	limit, offset, perr := parsePaging(r)
	if perr != "" {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", perr)
		return
	}

	jobs, err := h.store.ListJobs(r.Context(), tenantID, legalEntityID, sourceA, status, limit, offset)
	if err != nil {
		h.logger.Error("failed to list reconciliation jobs", zap.Error(err))
		problem.Write(w, r, http.StatusInternalServerError, "internal_error", "failed to query reconciliation jobs")
		return
	}

	if jobs == nil {
		jobs = []domain.ReconciliationJob{}
	}
	// The list is a summary: suggestions are read per job via GET /{id}.
	for i := range jobs {
		jobs[i].UnmatchedItems = nil
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"data":   jobs,
		"count":  len(jobs),
		"limit":  limit,
		"offset": offset,
	})
}

// parsePaging reads limit (default 50, 1..200) and offset (>= 0). Lists are
// always bounded: an omitted limit is the default, never "every row".
func parsePaging(r *http.Request) (limit, offset int, problem string) {
	limit, offset = domain.DefaultJobListLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > domain.MaxJobListLimit {
			return 0, 0, fmt.Sprintf("limit must be an integer between 1 and %d", domain.MaxJobListLimit)
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, 0, "offset must be a non-negative integer"
		}
		offset = n
	}
	return limit, offset, ""
}

// GetJobByID is authorized against the job's own legal entity, in the same
// order the write routes use: load (tenant-scoped by RLS), then check.
func (h *Handler) GetJobByID(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.writeJobLoadErr(w, r, err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), tenantID, principalID, job.LegalEntityID, RECONCILIATION_READ); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	setETag(w, job.Version)
	h.respondJSON(w, http.StatusOK, job)
}

func (h *Handler) ApplyResolution(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	jobID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	// Item ids are UUIDs; a malformed one is the caller's 400, never a
	// database cast error relayed back.
	itemID, ok := pathUUID(w, r, "itemId")
	if !ok {
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req domain.ApplyResolutionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		problem.Write(w, r, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}

	// A decision names its outcome explicitly: a missing status is refused,
	// never read as approval.
	if req.SuggestionStatus == "" {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", "suggestion_status is required")
		return
	}
	if err := domain.ValidateResolutionTarget(req.SuggestionStatus); err != nil {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	// ZS-SVC-Z-001 INV-18: a manual decision carries its reason.
	if strings.TrimSpace(req.ResolutionNotes) == "" {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", "resolution_notes (the reason for the decision) is required")
		return
	}
	if err := domain.ValidateAuthoritativeRef(req.SuggestionStatus, req.AuthoritativeRef); err != nil {
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.writeJobLoadErr(w, r, err)
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

	item, err := h.store.ApplyResolution(r.Context(), tenantID, jobID, itemID, req.SuggestionStatus, req.ResolutionNotes,
		req.AuthoritativeRef, job.LegalEntityID, principalID, r.Header.Get("X-Correlation-ID"), expectedVersion)
	if err != nil {
		h.writeCommandErr(w, r, err)
		return
	}

	h.respondJSON(w, http.StatusOK, item)
}

func (h *Handler) ArchiveJob(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	expectedVersion, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	job, err := h.store.GetJobByID(r.Context(), tenantID, id)
	if err != nil {
		h.writeJobLoadErr(w, r, err)
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

	archived, err := h.store.ArchiveJob(r.Context(), tenantID, id, principalID, r.Header.Get("X-Correlation-ID"), expectedVersion)
	if err != nil {
		h.writeCommandErr(w, r, err)
		return
	}

	setETag(w, archived.Version)
	h.respondJSON(w, http.StatusOK, archived)
}

// writeCommandErr maps the outcome of a decision or the archive.
func (h *Handler) writeCommandErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrJobNotFound):
		problem.Write(w, r, http.StatusNotFound, "job_not_found", "reconciliation job not found")
	case errors.Is(err, domain.ErrItemNotFound):
		problem.Write(w, r, http.StatusNotFound, "suggestion_not_found", "suggestion not found in this job")
	case errors.Is(err, domain.ErrJobArchived):
		problem.Write(w, r, http.StatusConflict, "job_archived", domain.ErrJobArchived.Error())
	case errors.Is(err, domain.ErrVersionMismatch):
		problem.Write(w, r, http.StatusPreconditionFailed, "version_mismatch", domain.ErrVersionMismatch.Error())
	case errors.Is(err, domain.ErrSegregationOfDuties):
		problem.Write(w, r, http.StatusForbidden, "segregation_of_duties", domain.ErrSegregationOfDuties.Error())
	case errors.Is(err, domain.ErrResolutionTransition):
		problem.Write(w, r, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrAuthoritativeRefRequired), errors.Is(err, domain.ErrInvalidAuthoritativeRef):
		// Whether a reference is required depends on the suggestion's
		// recommendation, which the store reads with the row it locks.
		problem.Write(w, r, http.StatusBadRequest, "validation_failed", err.Error())
	default:
		h.logger.Error("reconciliation command failed", zap.Error(err))
		problem.Write(w, r, http.StatusInternalServerError, "internal_error", "the command could not be completed")
	}
}

// writeJobLoadErr: a job that does not exist and another tenant's job (hidden
// by RLS) both read as not found.
func (h *Handler) writeJobLoadErr(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrJobNotFound) {
		problem.Write(w, r, http.StatusNotFound, "job_not_found", "reconciliation job not found")
		return
	}
	h.logger.Error("load reconciliation job failed", zap.Error(err))
	problem.Write(w, r, http.StatusInternalServerError, "internal_error", "could not load the reconciliation job")
}

// pathUUID reads a UUID path parameter; a malformed one is 400 invalid_id.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	raw := chi.URLParam(r, name)
	id, err := uuid.Parse(raw)
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "invalid_id", name+" must be a UUID")
		return "", false
	}
	return id.String(), true
}

// parseIfMatch reads an optional If-Match carrying the job version as a
// strong ETag ("3"). Absent means no precondition (0); malformed is 400.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, true
	}
	v, err := strconv.Atoi(strings.Trim(raw, `"`))
	if err != nil || v < 1 || len(raw) < 3 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		problem.Write(w, r, http.StatusBadRequest, "invalid_if_match", `If-Match must be the job version as a strong ETag, e.g. "3"`)
		return 0, false
	}
	return v, true
}

// setETag exposes the aggregate version for the If-Match contract.
func setETag(w http.ResponseWriter, version int) {
	if version > 0 {
		w.Header().Set("ETag", fmt.Sprintf("%q", strconv.Itoa(version)))
	}
}

func (h *Handler) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

// requirePrincipal reads the caller's identity from X-Principal-Id, set by
// the gateway after identity verification. A request with no resolved
// principal never passed identity verification — fail closed with 401.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		problem.Write(w, r, http.StatusUnauthorized, "missing_principal", "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr maps an authz.CheckAllowed error to the appropriate HTTP
// response. Denial is 403; any other error (including authorization-svc
// being unreachable) is 503 — fail closed, never allow silently.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		problem.Write(w, nil, http.StatusForbidden, "authorization_denied", "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	problem.Write(w, nil, http.StatusServiceUnavailable, "authorization_unavailable", "authorization service unavailable")
}

// writeBankRecErr maps a bank-reconciliation-svc read failure. Every case
// refuses the analysis — no suggestion is ever produced from a population
// that could not be read completely and exactly.
func (h *Handler) writeBankRecErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, bankrec.ErrRunNotFound):
		problem.Write(w, nil, http.StatusNotFound, "run_not_found", "bank reconciliation run not found")
	case errors.Is(err, bankrec.ErrPopulationTooLarge):
		problem.Write(w, nil, http.StatusUnprocessableEntity, "population_too_large", err.Error())
	case errors.Is(err, bankrec.ErrServiceUnavailable):
		h.logger.Error("bank-reconciliation-svc read failed", zap.Error(err))
		problem.Write(w, nil, http.StatusServiceUnavailable, "bank_reconciliation_unavailable", "bank-reconciliation-svc unavailable")
	default:
		// ErrRequestRefused, ErrInconsistent, ErrScopeMissing: the upstream
		// answered, but not with something this service can stand behind.
		h.logger.Error("bank-reconciliation-svc read failed", zap.Error(err))
		problem.Write(w, nil, http.StatusBadGateway, "bank_reconciliation_unusable", "bank-reconciliation-svc returned an unusable response")
	}
}

// readCandidates reads, for each distinct cash account the run's lines post
// to, that account's FINALIZED ledger entries in a window around the
// statement date — one read per account, not per line. Every line of a run
// shares one bank account and one statement date, so this is normally a
// single read. The window is the search horizon, never narrower than the
// governed date tolerance.
func (h *Handler) readCandidates(ctx context.Context, scope ledger.Scope, lines []domain.BankStatementLine, tol *domain.DateTolerance) (*domain.CandidateSearch, error) {
	horizon := h.searchHorizonDays
	if tol.Days > horizon {
		horizon = tol.Days
	}
	search := &domain.CandidateSearch{Tolerance: *tol, HorizonDays: horizon, EntriesByAccount: map[string][]domain.LedgerEntry{}}

	type window struct{ account, date string }
	seen := map[window]bool{}
	for _, l := range lines {
		if l.GLCashAccountCode == "" {
			continue
		}
		wnd := window{l.GLCashAccountCode, l.StatementDate}
		if seen[wnd] {
			continue
		}
		seen[wnd] = true
		date, err := time.Parse("2006-01-02", l.StatementDate)
		if err != nil {
			return nil, fmt.Errorf("%w: statement line %s date %q", ledger.ErrInconsistent, l.StatementLineID, l.StatementDate)
		}
		from := date.AddDate(0, 0, -horizon).Format("2006-01-02")
		to := date.AddDate(0, 0, horizon).Format("2006-01-02")
		entries, err := h.ledger.ListCashAccountEntries(ctx, scope, l.GLCashAccountCode, from, to)
		if err != nil {
			return nil, err
		}
		search.EntriesByAccount[l.GLCashAccountCode] = append(search.EntriesByAccount[l.GLCashAccountCode], entries...)
	}
	return search, nil
}

// writeLedgerErr responds to a failed candidate-search read. Nothing is
// analyzed without it: a missing read would turn "could not look" into
// "no candidate", which is a false statement.
func (h *Handler) writeLedgerErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ledger.ErrWindowTooLarge):
		problem.Write(w, nil, http.StatusUnprocessableEntity, "ledger_window_too_large", "the cash account has too many ledger entries in the candidate search window to read completely")
	case errors.Is(err, ledger.ErrServiceUnavailable):
		h.logger.Error("general-ledger-svc candidate read failed", zap.Error(err))
		problem.Write(w, nil, http.StatusServiceUnavailable, "general_ledger_unavailable", "general-ledger-svc unavailable")
	default:
		h.logger.Error("general-ledger-svc candidate read was refused or inconsistent", zap.Error(err))
		problem.Write(w, nil, http.StatusBadGateway, "general_ledger_unusable", "general-ledger-svc returned ledger entries that could not be used")
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
		problem.Write(w, nil, http.StatusFailedDependency, "tolerance_not_configured", "no governed tolerance policy configured for metric "+metric)
		return
	}
	if errors.Is(err, financialcontrol.ErrUnsupportedDateBasis) {
		problem.Write(w, nil, http.StatusFailedDependency, "tolerance_date_basis_unsupported", "governed tolerance policy for metric "+metric+" uses date_basis BUSINESS, which has no business-day calendar to count against; configure CALENDAR")
		return
	}
	h.logger.Error("financial-control-svc tolerance lookup failed", zap.String("metric", metric), zap.Error(err))
	problem.Write(w, nil, http.StatusServiceUnavailable, "financial_control_unavailable", "financial-control-svc unavailable")
}
