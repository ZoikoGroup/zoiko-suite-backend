// Package handler is the HTTP surface for reconciliation-engine-svc
// (DATA-07, ZS-SVC-N-001 §4). Every write goes through the store, which
// already writes its own outbox_events row in the same transaction as
// the business write — this layer never publishes directly.
//
// Authorization is evaluated against the legal entity the definition or run
// belongs to (A10), never the tenant id: the entity is resolved from the
// stored record before the authorization call, so a caller cannot choose
// which entity a check is made against.
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"

	"zoiko.io/reconciliation-engine-svc/internal/authz"
	"zoiko.io/reconciliation-engine-svc/internal/domain"
	svcenvelope "zoiko.io/reconciliation-engine-svc/internal/envelope"
	"zoiko.io/reconciliation-engine-svc/internal/health"
	customMiddleware "zoiko.io/reconciliation-engine-svc/internal/middleware"
	"zoiko.io/reconciliation-engine-svc/internal/source"
	"zoiko.io/reconciliation-engine-svc/internal/store"
)

const (
	ActionReconciliationManage = "RECONCILIATION_MANAGE"
	ActionReconciliationRead   = "RECONCILIATION_READ"
	// ActionReconciliationAcceptRisk is the authority path for accepting an
	// exception as risk (ZS-SVC-Z-001 DQC-05; ZS-CONTROL-001 scenario 24 "waived
	// without authority — command rejected"). It is separate from MANAGE.
	ActionReconciliationAcceptRisk = "RECONCILIATION_ACCEPT_RISK"
	// ActionReconciliationApproveRules approves a rule version — separate from
	// publishing it (DATA-07 "rule publication separated from … approval").
	ActionReconciliationApproveRules = "RECONCILIATION_APPROVE_RULES"
	// ActionReconciliationCertify seals a run — separate from preparing it
	// (ZS-CONTROL-001 Invariant 12: a preparer cannot self-certify).
	ActionReconciliationCertify = "RECONCILIATION_CERTIFY"
)

// populationReadTimeout bounds reading both populations of a StartRun. The
// read runs on a context DETACHED from the HTTP request, so a client that
// disconnects mid-read does not abandon a run that was about to be frozen;
// its retry with the same Idempotency-Key replays the run instead.
const populationReadTimeout = 3 * time.Minute

// persistTimeout bounds the write that follows the population read — the
// frozen run, or the Failed one. It gets its own detached budget, so a read
// that used all of populationReadTimeout can still be recorded. Read plus
// write stays inside the server's WriteTimeout.
const persistTimeout = 30 * time.Second

// Authorizer is the authorization-svc check this handler needs.
type Authorizer interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store   store.Store
	authz   Authorizer
	fetcher source.Fetcher
	logger  *zap.Logger
}

func NewHandler(s store.Store, a Authorizer, f source.Fetcher, l *zap.Logger) *Handler {
	return &Handler{store: s, authz: a, fetcher: f, logger: l}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.Handler())

	r.With(customMiddleware.TenantMiddleware).Route("/v1/reconciliation", func(r chi.Router) {
		h.routes(r)
	})

	return r
}

func (h *Handler) routes(r chi.Router) {
	r.Post("/definitions", h.DefineReconciliation)
	r.Get("/definitions/{definitionID}", h.GetDefinition)
	r.Post("/definitions/{definitionID}/rule-versions", h.CreateRuleVersion)
	r.Get("/definitions/{definitionID}/rule-versions", h.ListRuleVersions)
	r.Post("/definitions/{definitionID}/rule-versions/{version}/approve", h.ApproveRuleVersion)
	r.Post("/runs", h.StartRun)
	r.Get("/runs/{runID}", h.GetRun)
	r.Post("/runs/{runID}:match", h.Match)
	r.Post("/runs/{runID}:raise-exception", h.RaiseException)
	r.Post("/runs/{runID}:reperform", h.Reperform)
	r.Post("/runs/{runID}:certify", h.Certify)
	r.Post("/runs/{runID}:supersede", h.SupersedeRun)
	r.Get("/runs/{runID}/populations", h.GetPopulationSnapshots)
	r.Get("/runs/{runID}/matches", h.GetMatchResults)
	r.Get("/runs/{runID}/exceptions", h.GetExceptions)
	r.Get("/runs/{runID}/certification", h.GetCertification)
	r.Get("/runs/{runID}/transitions", h.GetTransitions)
	r.Get("/runs/{runID}/assessment", h.GetAssessment)
	r.Get("/exceptions/{exceptionID}", h.GetException)
	r.Get("/exceptions/{exceptionID}/transitions", h.GetExceptionTransitions)
	r.Post("/exceptions/{exceptionID}:assign", h.AssignException)
	r.Post("/exceptions/{exceptionID}:transition", h.TransitionException)
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		h.respondError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, legalEntityID, action string) bool {
	if legalEntityID == "" {
		// A record without an entity cannot be authorized against one; never fall
		// back to a broader scope.
		h.respondFromError(w, domain.ErrLegacyRecord)
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// authorizeRun resolves the run's legal entity from the stored run and
// authorizes the caller against it. It returns the tenant and principal.
func (h *Handler) authorizeRun(w http.ResponseWriter, r *http.Request, action string) (tenantID, principalID string, ok bool) {
	tenantID = customMiddleware.GetTenantID(r.Context())
	principalID, ok = h.requirePrincipal(w, r)
	if !ok {
		return "", "", false
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return "", "", false
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, action) {
		return "", "", false
	}
	return tenantID, principalID, true
}

// authorizeException resolves an exception's run, and through it the legal
// entity, and authorizes the caller against that entity.
func (h *Handler) authorizeException(w http.ResponseWriter, r *http.Request, action string) (tenantID, principalID string, ok bool) {
	tenantID = customMiddleware.GetTenantID(r.Context())
	principalID, ok = h.requirePrincipal(w, r)
	if !ok {
		return "", "", false
	}
	e, err := h.store.GetException(r.Context(), tenantID, chi.URLParam(r, "exceptionID"))
	if err != nil {
		h.respondFromError(w, err)
		return "", "", false
	}
	run, err := h.store.GetRun(r.Context(), tenantID, e.RunID)
	if err != nil {
		h.respondFromError(w, err)
		return "", "", false
	}
	if !h.authorize(w, r, principalID, run.LegalEntityID, action) {
		return "", "", false
	}
	return tenantID, principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		h.respondError(w, http.StatusForbidden, "authorization denied")
		return
	}
	h.logger.Error("authorization check failed", zap.Error(err))
	h.respondError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func (h *Handler) readBody(r *http.Request, v interface{}) (string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, v); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (h *Handler) idempotencyClaim(principalID, operation, resourceID, requestSHA256, key string) domain.IdempotencyClaim {
	return domain.IdempotencyClaim{
		OwnerScope: domain.SellerScope, PrincipalID: principalID, Key: key,
		Operation: operation, RequestSHA256: requestSHA256, ResourceID: resourceID,
	}
}

// ── Commands ─────────────────────────────────────────────────────────────────

// readStrict decodes the body refusing unknown fields — a client still
// sending a removed field (tolerance_amount_minor_units, materiality_amount_
// minor_units) is told so instead of silently getting a different rule — and
// returns the body's hash for the idempotency claim.
func (h *Handler) readStrict(r *http.Request, v interface{}) (string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// DefineReconciliation creates a definition and its first rule version as a
// draft; it cannot run until someone else approves that version.
func (h *Handler) DefineReconciliation(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req domain.DefineReconciliationRequest
	reqHash, err := h.readStrict(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	// The entity is checked to be well-formed before it is used as the scope of
	// the authorization call.
	if err := req.Validate(); err != nil {
		h.respondFromError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, req.LegalEntityID, ActionReconciliationManage) {
		return
	}
	claim := h.idempotencyClaim(principalID, "DefineReconciliation", req.LegalEntityID+"|"+req.Name, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.DefineReconciliation(r.Context(), tenantID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

// authorizeDefinition loads the definition and authorizes the caller against
// its legal entity.
func (h *Handler) authorizeDefinition(w http.ResponseWriter, r *http.Request, action string) (*domain.ReconciliationDefinition, string, string, bool) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, "", "", false
	}
	def, err := h.store.GetDefinition(r.Context(), tenantID, chi.URLParam(r, "definitionID"))
	if err != nil {
		h.respondFromError(w, err)
		return nil, "", "", false
	}
	if !h.authorize(w, r, principalID, def.LegalEntityID, action) {
		return nil, "", "", false
	}
	return def, tenantID, principalID, true
}

// GetDefinition returns the definition with its effective rules (the latest
// approved version in effect; none → version 0) and its latest version number.
func (h *Handler) GetDefinition(w http.ResponseWriter, r *http.Request) {
	def, _, _, ok := h.authorizeDefinition(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	h.respondJSON(w, http.StatusOK, def)
}

// CreateRuleVersion publishes a draft rule version based on the definition's
// latest version.
func (h *Handler) CreateRuleVersion(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRuleVersionRequest
	reqHash, err := h.readStrict(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		h.respondFromError(w, err)
		return
	}
	def, tenantID, principalID, ok := h.authorizeDefinition(w, r, ActionReconciliationManage)
	if !ok {
		return
	}
	claim := h.idempotencyClaim(principalID, "CreateRuleVersion", def.DefinitionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CreateRuleVersion(r.Context(), tenantID, def.DefinitionID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) ListRuleVersions(w http.ResponseWriter, r *http.Request) {
	def, tenantID, _, ok := h.authorizeDefinition(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	items, err := h.store.ListRuleVersions(r.Context(), tenantID, def.DefinitionID)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.RuleVersion{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

// ApproveRuleVersion records the independent approval of a draft. Authority is
// evaluated at the moment of approval, against the definition's entity.
func (h *Handler) ApproveRuleVersion(w http.ResponseWriter, r *http.Request) {
	version, err := strconv.ParseInt(chi.URLParam(r, "version"), 10, 64)
	if err != nil || version < 1 {
		h.respondError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	def, tenantID, principalID, ok := h.authorizeDefinition(w, r, ActionReconciliationApproveRules)
	if !ok {
		return
	}
	claim := h.idempotencyClaim(principalID, "ApproveRuleVersion", def.DefinitionID+"|"+chi.URLParam(r, "version"), "",
		r.Header.Get("Idempotency-Key"))
	got, err := h.store.ApproveRuleVersion(r.Context(), tenantID, def.DefinitionID, version, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// StartRun reads the definition's two populations from their owning services
// and freezes them (A1). Nothing is frozen unless both populations are proven
// complete and reproducible. When they cannot be — a missing watermark, absent
// or untied declared totals, an unreachable source, an amount the definition's
// scale cannot represent — the attempt is still recorded, as a Failed run with
// result INDETERMINATE and the failure's code (A6), and the response is 502
// (source unreachable) or 422 with that run.
func (h *Handler) StartRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req domain.StartRunRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.respondFromError(w, err)
		return
	}
	def, err := h.store.GetDefinition(r.Context(), tenantID, req.DefinitionID)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, def.LegalEntityID, ActionReconciliationManage) {
		return
	}
	if err := def.Runnable(); err != nil {
		h.respondFromError(w, err)
		return
	}
	claim := h.idempotencyClaim(principalID, "StartRun", req.DefinitionID, reqHash, r.Header.Get("Idempotency-Key"))
	if err := h.store.CheckIdempotency(r.Context(), tenantID, claim); err != nil {
		h.respondFromError(w, err)
		return
	}

	readCtx, cancelRead := context.WithTimeout(context.WithoutCancel(r.Context()), populationReadTimeout)
	defer cancelRead()
	scope := source.Scope{TenantID: tenantID, PrincipalID: principalID, LegalEntityID: def.LegalEntityID,
		PeriodID: req.PeriodID, CorrelationID: r.Header.Get("X-Correlation-ID")}
	pops, err := source.BuildPopulations(readCtx, h.fetcher, *def, req, scope)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), persistTimeout)
	defer cancel()
	var popErr *source.PopulationError
	if errors.As(err, &popErr) {
		h.recordFailedRun(ctx, w, tenantID, req, def.Version, popErr, principalID, claim)
		return
	}
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	got, err := h.store.StartRun(ctx, tenantID, req, def.Version, pops, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) Match(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.authorizeRun(w, r, ActionReconciliationManage)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.MatchRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "Match", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.Match(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) RaiseException(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.authorizeRun(w, r, ActionReconciliationManage)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.RaiseExceptionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "RaiseException", runID+"|"+req.ItemID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RaiseException(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) Reperform(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.authorizeRun(w, r, ActionReconciliationManage)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principalID, "Reperform", runID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.Reperform(r.Context(), tenantID, runID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) Certify(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.authorizeRun(w, r, ActionReconciliationCertify)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "runID")
	claim := h.idempotencyClaim(principalID, "Certify", runID, "", r.Header.Get("Idempotency-Key"))
	got, err := h.store.Certify(r.Context(), tenantID, runID, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) SupersedeRun(w http.ResponseWriter, r *http.Request) {
	tenantID, principalID, ok := h.authorizeRun(w, r, ActionReconciliationManage)
	if !ok {
		return
	}
	runID := chi.URLParam(r, "runID")
	var req domain.SupersedeRunRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	claim := h.idempotencyClaim(principalID, "SupersedeRun", runID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.SupersedeRun(r.Context(), tenantID, runID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Read/query surfaces ──────────────────────────────────────────────────────

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	got, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if !h.authorize(w, r, principalID, got.LegalEntityID, ActionReconciliationRead) {
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetPopulationSnapshots(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	items, err := h.store.GetPopulationSnapshots(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.PopulationSnapshot{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetMatchResults(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	items, err := h.store.GetMatchResults(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.MatchResult{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetExceptions(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	items, err := h.store.GetExceptions(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.ReconciliationException{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) GetCertification(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	got, err := h.store.GetCertification(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// GetTransitions returns the run's append-only state history: every change to
// its lifecycle, result or certification, with reason and actor.
func (h *Handler) GetTransitions(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	items, err := h.store.GetTransitions(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.RunTransition{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

// GetAssessment is what the run's result rests on: unresolved exceptions by
// state, every accepted risk, aggregate materiality per currency, and whether
// (and why not) the run can be certified.
func (h *Handler) GetAssessment(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeRun(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	got, err := h.store.GetAssessment(r.Context(), tenantID, chi.URLParam(r, "runID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetException(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeException(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	got, err := h.store.GetException(r.Context(), tenantID, chi.URLParam(r, "exceptionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

func (h *Handler) GetExceptionTransitions(w http.ResponseWriter, r *http.Request) {
	tenantID, _, ok := h.authorizeException(w, r, ActionReconciliationRead)
	if !ok {
		return
	}
	items, err := h.store.GetExceptionTransitions(r.Context(), tenantID, chi.URLParam(r, "exceptionID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	if items == nil {
		items = []domain.ExceptionTransition{}
	}
	h.respondJSON(w, http.StatusOK, map[string]interface{}{"data": items, "count": len(items)})
}

func (h *Handler) AssignException(w http.ResponseWriter, r *http.Request) {
	var req domain.AssignExceptionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tenantID, principalID, ok := h.authorizeException(w, r, ActionReconciliationManage)
	if !ok {
		return
	}
	exceptionID := chi.URLParam(r, "exceptionID")
	claim := h.idempotencyClaim(principalID, "AssignException", exceptionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.AssignException(r.Context(), tenantID, exceptionID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// TransitionException moves an exception along its lifecycle. Accepting risk
// needs the separate RECONCILIATION_ACCEPT_RISK permission; the store also
// requires the acceptor to be neither the exception's owner nor its raiser.
func (h *Handler) TransitionException(w http.ResponseWriter, r *http.Request) {
	var req domain.TransitionExceptionRequest
	reqHash, err := h.readBody(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := req.Validate(); err != nil {
		h.respondFromError(w, err)
		return
	}
	action := ActionReconciliationManage
	if req.ToState == domain.ExceptionAcceptedRisk {
		action = ActionReconciliationAcceptRisk
	}
	tenantID, principalID, ok := h.authorizeException(w, r, action)
	if !ok {
		return
	}
	exceptionID := chi.URLParam(r, "exceptionID")
	claim := h.idempotencyClaim(principalID, "TransitionException", exceptionID, reqHash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.TransitionException(r.Context(), tenantID, exceptionID, req, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// ── Response helpers ─────────────────────────────────────────────────────────

func (h *Handler) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) respondError(w http.ResponseWriter, code int, message string) {
	h.respondJSON(w, code, map[string]string{"error": message})
}

// recordFailedRun answers a StartRun whose populations could not be proven
// complete and reproducible: the attempt is recorded as a Failed /
// INDETERMINATE run, and the response carries it. An unreachable source is a
// gateway failure (502); a source that answered with something that does not
// hold together is 422. If even the failed run cannot be recorded (the
// definition changed meanwhile, the database is down), that error is returned
// instead and no run exists.
func (h *Handler) recordFailedRun(ctx context.Context, w http.ResponseWriter, tenantID string, req domain.StartRunRequest,
	definitionVersion int64, popErr *source.PopulationError, principalID string, claim domain.IdempotencyClaim) {
	h.logger.Warn("reconciliation populations could not be proven complete; recording a failed run", zap.Error(popErr))
	failure := domain.RunFailure{Code: popErr.Code(), Reason: popErr.Error()}
	run, err := h.store.RecordFailedRun(ctx, tenantID, req, definitionVersion, failure, principalID, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	code := http.StatusUnprocessableEntity
	if failure.Code == domain.FailureSourceUnavailable {
		code = http.StatusBadGateway
	}
	h.respondJSON(w, code, map[string]interface{}{
		"error":        popErr.Error(),
		"result_state": run.ResultState,
		"failure_code": failure.Code,
		"run":          run,
	})
}

func (h *Handler) respondFromError(w http.ResponseWriter, err error) {
	var replay *domain.IdempotentReplayError
	if errors.As(err, &replay) {
		w.Header().Set("Idempotent-Replayed", "true")
		h.respondJSON(w, http.StatusOK, map[string]string{"resource_id": replay.ResourceID})
		return
	}
	switch {
	case errors.Is(err, domain.ErrNotExceptionOwner), errors.Is(err, domain.ErrNotIndependent), errors.Is(err, domain.ErrSelfApproval),
		errors.Is(err, domain.ErrNotIndependentCertifier):
		h.respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrDefinitionNotFound), errors.Is(err, domain.ErrRunNotFound),
		errors.Is(err, domain.ErrItemNotFound), errors.Is(err, domain.ErrExceptionNotFound), errors.Is(err, domain.ErrRuleVersionNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrRunNotOpen), errors.Is(err, domain.ErrOpenExceptionsRemain),
		errors.Is(err, domain.ErrRunImmutable), errors.Is(err, domain.ErrExceptionAlreadyClosed),
		errors.Is(err, domain.ErrIdempotencyKeyReused), errors.Is(err, domain.ErrDefinitionChanged),
		errors.Is(err, domain.ErrLegacyRecord), errors.Is(err, domain.ErrSupersedeDifferentDefinition),
		errors.Is(err, domain.ErrReplacementNotTerminal), errors.Is(err, domain.ErrReplacementAlreadyUsed),
		errors.Is(err, domain.ErrInvalidTransition), errors.Is(err, domain.ErrRunNotEvaluated),
		errors.Is(err, domain.ErrRunNotEligible), errors.Is(err, domain.ErrNoEffectiveRules),
		errors.Is(err, domain.ErrDefinitionExists), errors.Is(err, domain.ErrRuleVersionConflict),
		errors.Is(err, domain.ErrRuleVersionApproved), errors.Is(err, domain.ErrBackdatedRuleVersion):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		h.logger.Error("reconciliation-engine-svc request failed", zap.Error(err))
		h.respondError(w, http.StatusBadRequest, err.Error())
	}
}
