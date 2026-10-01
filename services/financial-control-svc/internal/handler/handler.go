// Package handler exposes financial-control-svc's /controls/v1 API
// (ZS-CONTROL-001 §25).
//
// Command rules enforced here: idempotency on run creation, server-side scope
// resolution (the body is validated against the verified tenant, never
// trusted), authorization re-evaluated on every material command, and no
// route that can mutate ledger state — this service has no ledger client.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/financial-control-svc/internal/domain"
	"zoiko.io/financial-control-svc/internal/engine"
	svcmiddleware "zoiko.io/financial-control-svc/internal/middleware"
	"zoiko.io/financial-control-svc/internal/store"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateDefinition(ctx context.Context, tenantID, actor, correlationID string, req domain.CreateControlDefinitionRequest) (*domain.ControlDefinition, error)
	GetDefinition(ctx context.Context, tenantID, id string) (*domain.ControlDefinition, error)
	ListDefinitions(ctx context.Context, tenantID string, limit int) ([]domain.ControlDefinition, error)
	CreateRuleVersion(ctx context.Context, tenantID, definitionID, actor string, req domain.CreateRuleVersionRequest) (*domain.ControlRuleVersion, error)
	ApproveRuleVersion(ctx context.Context, tenantID, definitionID string, version int, approver string) (*domain.ControlRuleVersion, error)
	ListRuleVersions(ctx context.Context, tenantID, definitionID string) ([]domain.ControlRuleVersion, error)
	CreateTolerancePolicy(ctx context.Context, tenantID, actor string, req domain.CreateTolerancePolicyRequest) (*domain.TolerancePolicy, error)
	ListTolerancePolicies(ctx context.Context, tenantID, legalEntityID, metric string) ([]domain.TolerancePolicy, error)
	CreateMaterialityPolicy(ctx context.Context, tenantID, actor string, req domain.CreateMaterialityPolicyRequest) (*domain.MaterialityPolicy, error)
	CreateRun(ctx context.Context, tenantID, actor, correlationID, idempotencyKey string, req domain.CreateRunRequest) (*domain.ControlRun, bool, error)
	GetRun(ctx context.Context, tenantID, runID string) (*domain.ControlRun, error)
	ListRuns(ctx context.Context, tenantID string, f domain.ListRunsFilter) ([]domain.ControlRun, error)
	ListTransitions(ctx context.Context, tenantID, runID string) ([]domain.Transition, error)

	// Wave 1.
	BeginExecution(ctx context.Context, tenantID, runID, idempotencyKey, actor, correlationID string, expectedVersion int) (*domain.ControlRun, bool, error)
	ListPopulationSnapshots(ctx context.Context, tenantID, runID string) ([]domain.PopulationSnapshot, error)
	ListPopulationRecords(ctx context.Context, tenantID, runID string, side domain.Side, afterSeq int64, limit int) ([]store.PopulationRecordRow, error)
	ListExceptions(ctx context.Context, tenantID, runID string, f store.ListExceptionsFilter) ([]domain.ControlException, error)
	GetException(ctx context.Context, tenantID, exceptionID string) (*domain.ControlException, error)
	AssignException(ctx context.Context, tenantID, exceptionID, actor, correlationID string, expectedVersion int, req domain.AssignExceptionRequest) (*domain.ControlException, error)
	GetLatestEvidence(ctx context.Context, tenantID, runID string) (*domain.EvidencePackage, error)

	// Wave 5.
	CertifyRun(ctx context.Context, tenantID, runID, actor, correlationID string, expectedVersion int, req domain.CertifyRequest) (*domain.ControlRun, error)
	CloseGate(ctx context.Context, tenantID, legalEntityID, periodID string) (*domain.CloseGate, error)
	ExceptionSummary(ctx context.Context, tenantID, legalEntityID, periodID string) (*domain.ExceptionSummary, error)

	// Wave 7.
	ResolveException(ctx context.Context, tenantID, exceptionID, actor, correlationID string, expectedVersion int, req domain.ResolveExceptionRequest) (*domain.ControlException, error)
	ListExceptionTransitions(ctx context.Context, tenantID, exceptionID string) ([]domain.ExceptionTransition, error)
	SubmitForCertification(ctx context.Context, tenantID, runID, actor, correlationID string, expectedVersion int, reason string) (*domain.ControlRun, error)

	// Wave 7/8.
	Monitoring(ctx context.Context, tenantID, legalEntityID, periodID string, now time.Time) (*domain.MonitoringSnapshot, error)
}

// Executor runs the control pipeline for a run that has been started.
type Executor interface {
	Execute(ctx context.Context, in engine.Input) (*domain.ControlRun, error)
}

// AuthZClient is the authorization contract (fail-closed).
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc.
const (
	ActionDefine       = "FINCTRL_DEFINE"
	ActionApproveRule  = "FINCTRL_APPROVE_RULE"
	ActionPolicyManage = "FINCTRL_POLICY_MANAGE"
	ActionRunCreate    = "FINCTRL_RUN_CREATE"
	ActionRead         = "FINCTRL_READ"
	ActionExecute      = "FINCTRL_EXECUTE"
	ActionAssign       = "FINCTRL_EXCEPTION_ASSIGN"
)

// platformEntity is authorization-svc's sentinel for an act that belongs to no
// single legal entity (control definitions are tenant-level).
const platformEntity = "PLATFORM"

const maxBodyBytes = 256 << 10

type Handler struct {
	store    Store
	authz    AuthZClient
	executor Executor
	log      *zap.Logger
}

func New(s Store, a AuthZClient, ex Executor, log *zap.Logger) *Handler {
	return &Handler{store: s, authz: a, executor: ex, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/controls/v1", func(r chi.Router) {
		r.Route("/definitions", func(r chi.Router) {
			r.Post("/", h.CreateDefinition)
			r.Get("/", h.ListDefinitions)
			r.Get("/{definition_id}", h.GetDefinition)
			r.Post("/{definition_id}/rule-versions", h.CreateRuleVersion)
			r.Get("/{definition_id}/rule-versions", h.ListRuleVersions)
			r.Post("/{definition_id}/rule-versions/{version}/approve", h.ApproveRuleVersion)
		})
		r.Route("/tolerance-policies", func(r chi.Router) {
			r.Post("/", h.CreateTolerancePolicy)
			r.Get("/", h.ListTolerancePolicies)
		})
		r.Post("/materiality-policies", h.CreateMaterialityPolicy)
		r.Route("/runs", func(r chi.Router) {
			r.Post("/", h.CreateRun)
			r.Get("/", h.ListRuns)
			r.Get("/{run_id}", h.GetRun)
			r.Get("/{run_id}/transitions", h.ListTransitions)
			r.Post("/{run_id}/execute", h.ExecuteRun)
			r.Get("/{run_id}/population", h.GetPopulation)
			r.Get("/{run_id}/exceptions", h.ListExceptions)
			r.Get("/{run_id}/evidence", h.GetEvidence)
			r.Post("/{run_id}/certification", h.CertifyRun)
			r.Post("/{run_id}/submit-for-certification", h.SubmitForCertification)
			r.Get("/{run_id}/evidence-export", h.ExportRunEvidence)
		})
		r.Post("/exceptions/{exception_id}/assign", h.AssignException)
		r.Post("/exceptions/{exception_id}/transition", h.ResolveException)
		r.Get("/exceptions/{exception_id}/transitions", h.ListExceptionTransitions)
		r.Get("/close-gate", h.CloseGate)
		r.Get("/exception-summary", h.ExceptionSummary)
		r.Get("/monitoring/metrics", h.Monitoring)
		r.Post("/catalogue/seed", h.SeedCatalogue)
		r.Get("/catalogue/status", h.CatalogueStatus)
	})
}

// ─── Definitions ─────────────────────────────────────────────────────────────

func (h *Handler) CreateDefinition(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req domain.CreateControlDefinitionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		h.writeErr(w, "create definition", err)
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionDefine) {
		return
	}
	d, err := h.store.CreateDefinition(r.Context(), tenantID, principal, corrID(r), req)
	if err != nil {
		h.writeErr(w, "create definition", err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) GetDefinition(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionRead) {
		return
	}
	d, err := h.store.GetDefinition(r.Context(), tenantID, chi.URLParam(r, "definition_id"))
	if err != nil {
		h.writeErr(w, "get definition", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) ListDefinitions(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionRead) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.store.ListDefinitions(r.Context(), tenantID, limit)
	if err != nil {
		h.writeErr(w, "list definitions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

func (h *Handler) CreateRuleVersion(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req domain.CreateRuleVersionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		h.writeErr(w, "create rule version", err)
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionDefine) {
		return
	}
	v, err := h.store.CreateRuleVersion(r.Context(), tenantID, chi.URLParam(r, "definition_id"), principal, req)
	if err != nil {
		h.writeErr(w, "create rule version", err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (h *Handler) ListRuleVersions(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionRead) {
		return
	}
	list, err := h.store.ListRuleVersions(r.Context(), tenantID, chi.URLParam(r, "definition_id"))
	if err != nil {
		h.writeErr(w, "list rule versions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

func (h *Handler) ApproveRuleVersion(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	version, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil || version < 1 {
		writeError(w, http.StatusBadRequest, "invalid_identifier", domain.ErrInvalidIdentifier.Error())
		return
	}
	// Authority is re-evaluated at the moment of approval (Invariant 11).
	if !h.authorize(w, r, principal, platformEntity, ActionApproveRule) {
		return
	}
	v, err := h.store.ApproveRuleVersion(r.Context(), tenantID, chi.URLParam(r, "definition_id"), version, principal)
	if err != nil {
		h.writeErr(w, "approve rule version", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ─── Policies ────────────────────────────────────────────────────────────────

func (h *Handler) CreateTolerancePolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req domain.CreateTolerancePolicyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	}
	if err := req.Validate(principal); err != nil {
		h.writeErr(w, "create tolerance policy", err)
		return
	}
	if !h.authorize(w, r, principal, req.LegalEntityID, ActionPolicyManage) {
		return
	}
	p, err := h.store.CreateTolerancePolicy(r.Context(), tenantID, principal, req)
	if err != nil {
		h.writeErr(w, "create tolerance policy", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) ListTolerancePolicies(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	entity := r.URL.Query().Get("legal_entity_id")
	if entity == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	}
	if !h.authorize(w, r, principal, entity, ActionRead) {
		return
	}
	list, err := h.store.ListTolerancePolicies(r.Context(), tenantID, entity, r.URL.Query().Get("metric"))
	if err != nil {
		h.writeErr(w, "list tolerance policies", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

func (h *Handler) CreateMaterialityPolicy(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req domain.CreateMaterialityPolicyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	}
	if err := req.Validate(principal); err != nil {
		h.writeErr(w, "create materiality policy", err)
		return
	}
	if !h.authorize(w, r, principal, req.LegalEntityID, ActionPolicyManage) {
		return
	}
	p, err := h.store.CreateMaterialityPolicy(r.Context(), tenantID, principal, req)
	if err != nil {
		h.writeErr(w, "create materiality policy", err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// ─── Runs ────────────────────────────────────────────────────────────────────

// CreateRun — POST /controls/v1/runs. Idempotency-Key is mandatory (§25). The
// response carries the run's ETag; tolerance and rule version are pinned by the
// server and cannot be supplied or widened by the caller (scenario 04).
func (h *Handler) CreateRun(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key header is required to create a run")
		return
	}
	var req domain.CreateRunRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		h.writeErr(w, "create run", err)
		return
	}
	if !h.authorize(w, r, principal, req.LegalEntityID, ActionRunCreate) {
		return
	}
	run, created, err := h.store.CreateRun(r.Context(), tenantID, principal, corrID(r), key, req)
	if err != nil {
		h.writeErr(w, "create run", err)
		return
	}
	setETag(w, run.Version)
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replay", "true")
	}
	writeJSON(w, status, run)
}

func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "run_id"))
	if err != nil {
		h.writeErr(w, "get run", err)
		return
	}
	// Authorize against the run's own entity, not a client-asserted one.
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionRead) {
		return
	}
	run.Attention = attentionSignals(*run, time.Now().UTC())
	setETag(w, run.Version)
	writeJSON(w, http.StatusOK, run)
}

func (h *Handler) ListRuns(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	entity := q.Get("legal_entity_id")
	if entity == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id")
		return
	}
	if !h.authorize(w, r, principal, entity, ActionRead) {
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	f := domain.ListRunsFilter{
		ControlDefinitionID: q.Get("control_definition_id"), LegalEntityID: entity,
		PeriodID: q.Get("period_id"), LifecycleState: q.Get("lifecycle_state"), Limit: limit,
	}
	if c := q.Get("cursor"); c != "" {
		t, id, err := decodeCursor(c)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		f.AfterCreatedAt, f.AfterRunID = &t, id
	}
	list, err := h.store.ListRuns(r.Context(), tenantID, f)
	if err != nil {
		h.writeErr(w, "list runs", err)
		return
	}
	resp := map[string]any{"items": nonNil(list)}
	if n := len(list); n > 0 && n == effectiveLimit(limit) {
		resp["next_cursor"] = encodeCursor(list[n-1].CreatedAt, list[n-1].RunID)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) ListTransitions(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	run, err := h.store.GetRun(r.Context(), tenantID, chi.URLParam(r, "run_id"))
	if err != nil {
		h.writeErr(w, "list transitions", err)
		return
	}
	if !h.authorize(w, r, principal, run.LegalEntityID, ActionRead) {
		return
	}
	list, err := h.store.ListTransitions(r.Context(), tenantID, run.RunID)
	if err != nil {
		h.writeErr(w, "list transitions", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": nonNil(list)})
}

// attentionSignals derives non-authoritative flags (§7). They are computed on
// read and never written back, so they cannot overwrite an authoritative state.
func attentionSignals(r domain.ControlRun, now time.Time) []string {
	var out []string
	switch r.LifecycleState {
	case domain.LifecycleFailed:
		out = append(out, "CONTROL_FAILED")
	case domain.LifecycleScheduled, domain.LifecyclePreparing:
		if now.Sub(r.CreatedAt) > 24*time.Hour {
			out = append(out, "CONTROL_STALE")
		}
	}
	if r.ResultState == domain.ResultIndeterminate {
		out = append(out, "RESULT_INDETERMINATE")
	}
	return out
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// caller resolves the verified tenant and principal. Both come from headers set
// by gateway-auth-svc; nothing in a request body can substitute for them.
func (h *Handler) caller(w http.ResponseWriter, r *http.Request) (tenantID, principal string, ok bool) {
	tenantID = svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", domain.ErrTenantScopeMissing.Error())
		return "", "", false
	}
	principal = r.Header.Get("X-Principal-Id")
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", "", false
	}
	return tenantID, principal, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principal, entity, action string) bool {
	err := h.authz.CheckAllowed(r.Context(), principal, entity, action)
	switch {
	case err == nil:
		return true
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	default:
		h.log.Error("authorization unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "authorization_unavailable", "")
	}
	return false
}

func (h *Handler) writeErr(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrNotOwner):
		writeError(w, http.StatusForbidden, "not_exception_owner", err.Error())
	case errors.Is(err, domain.ErrNotIndependent):
		writeError(w, http.StatusForbidden, "segregation_of_duties", err.Error())
	case errors.Is(err, domain.ErrSegregation):
		writeError(w, http.StatusForbidden, "segregation_of_duties", err.Error())
	case errors.Is(err, domain.ErrSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval", err.Error())
	case errors.Is(err, domain.ErrInvalidArgument), errors.Is(err, domain.ErrInvalidIdentifier):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrDuplicate):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "version_conflict", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrTenantScopeMissing):
		writeError(w, http.StatusUnauthorized, "tenant_scope_missing", err.Error())
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}

// corrID is the request's correlation id. ZS-EVENT-001 makes it mandatory for governed flows, so a
// caller that sends none gets a generated one rather than an event with a blank id.
func corrID(r *http.Request) string {
	if c := r.Header.Get("X-Correlation-ID"); c != "" {
		return c
	}
	return uuid.NewString()
}

func setETag(w http.ResponseWriter, version int) {
	w.Header().Set("ETag", `"`+strconv.Itoa(version)+`"`)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func effectiveLimit(l int) int {
	if l <= 0 || l > 200 {
		return 50
	}
	return l
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, detail string) {
	writeJSON(w, status, errorResponse{Error: code, Detail: detail})
}
