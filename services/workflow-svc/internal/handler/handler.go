package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/authz"
	"zoiko.io/workflow-svc/internal/domain"
)

// WorkflowStore is the narrow interface the handler depends on.
type WorkflowStore interface {
	CreateWorkflow(ctx context.Context, params domain.CreateWorkflowParams) (*domain.WorkflowInstance, []*domain.WorkflowStage, error)
	FindWorkflowByID(ctx context.Context, workflowInstanceID string) (*domain.WorkflowInstance, error)
	FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error)
	FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error)
	SubmitAction(ctx context.Context, params domain.SubmitActionParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error)
	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
}

// EventPublisher is the narrow interface the handler depends on.
type EventPublisher interface {
	PublishWorkflowStarted(ctx context.Context, w domain.WorkflowInstance) error
	PublishApprovalGranted(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage) error
	PublishApprovalRejected(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage) error
	PublishWorkflowEscalated(ctx context.Context, w domain.WorkflowInstance) error
	PublishWorkflowCompleted(ctx context.Context, w domain.WorkflowInstance) error
}

type Handler struct {
	store     WorkflowStore
	publisher EventPublisher
	authz     authz.Client
	log       *zap.Logger
}

func New(store WorkflowStore, publisher EventPublisher, authzClient authz.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authzClient, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/workflows", h.CreateWorkflow)
	r.Get("/v1/workflows/{workflow_instance_id}", h.GetWorkflow)
	r.Get("/v1/workflows/{workflow_instance_id}/next-approver", h.GetNextApprover)
	r.Post("/v1/workflows/{workflow_instance_id}/actions", h.SubmitAction)
	r.Post("/v1/workflows/{workflow_instance_id}/escalate", h.EscalateWorkflow)
	r.Post("/v1/workflows/{workflow_instance_id}/cancel", h.CancelWorkflow)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/workflows ───────────────────────────────────────────────────────

type createWorkflowRequest struct {
	TenantID      string                            `json:"tenant_id"`
	LegalEntityID string                            `json:"legal_entity_id"`
	WorkflowType  string                            `json:"workflow_type"`
	InitiatedBy   string                            `json:"initiated_by"`
	Stages        []domain.CreateWorkflowStageInput `json:"stages"`
}

func (req createWorkflowRequest) missingField() string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.WorkflowType == "":
		return "workflow_type"
	case req.InitiatedBy == "":
		return "initiated_by"
	default:
		return ""
	}
}

type workflowResponse struct {
	*domain.WorkflowInstance
	Stages []*domain.WorkflowStage `json:"stages"`
}

// CreateWorkflow handles POST /v1/workflows.
//
// Response: 201 created / 400 missing field or no stages / 503 unavailable.
func (h *Handler) CreateWorkflow(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if len(req.Stages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_stages"})
		return
	}
	for _, st := range req.Stages {
		if st.ApproverPrincipalID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "stages[].approver_principal_id"})
			return
		}
	}

	instance, stages, err := h.store.CreateWorkflow(r.Context(), domain.CreateWorkflowParams{
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, WorkflowType: req.WorkflowType,
		InitiatedBy: req.InitiatedBy, CorrelationID: correlationID, Stages: req.Stages,
	})
	if err != nil {
		if errors.Is(err, domain.ErrNoStages) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_stages"})
			return
		}
		h.log.Error("CreateWorkflow: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	if pubErr := h.publisher.PublishWorkflowStarted(r.Context(), *instance); pubErr != nil {
		h.log.Error("CreateWorkflow: failed to publish workflow.started", zap.String("correlation_id", correlationID), zap.Error(pubErr))
	}

	h.log.Info("workflow started",
		zap.String("workflow_instance_id", instance.WorkflowInstanceID),
		zap.String("workflow_type", instance.WorkflowType),
		zap.Int("stage_count", len(stages)),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, workflowResponse{WorkflowInstance: instance, Stages: stages})
}

// ── GET /v1/workflows/{id} ───────────────────────────────────────────────────

// GetWorkflow handles GET /v1/workflows/{workflow_instance_id}.
//
// Response: 200 instance + stages / 404 not found / 503 unavailable.
func (h *Handler) GetWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	instance, err := h.store.FindWorkflowByID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "GetWorkflow")
		return
	}
	stages, err := h.store.FindStagesByWorkflowID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "GetWorkflow")
		return
	}
	writeJSON(w, http.StatusOK, workflowResponse{WorkflowInstance: instance, Stages: stages})
}

// ── GET /v1/workflows/{id}/next-approver ─────────────────────────────────────

// GetNextApprover handles GET /v1/workflows/{workflow_instance_id}/next-approver
// — the "resolve next approver" capability.
//
// Response: 200 the current stage / 404 not found or workflow already terminal / 503 unavailable.
func (h *Handler) GetNextApprover(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	stage, err := h.store.FindCurrentStage(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "GetNextApprover")
		return
	}
	writeJSON(w, http.StatusOK, stage)
}

// ── POST /v1/workflows/{id}/actions ──────────────────────────────────────────

type submitActionRequest struct {
	ActorPrincipalID string  `json:"actor_principal_id"`
	Action           string  `json:"action"`
	Rationale        *string `json:"rationale,omitempty"`
}

// SubmitAction handles POST /v1/workflows/{workflow_instance_id}/actions.
//
// Confirms via authorization-svc that the actor is authorized to submit an
// approval action before touching the workflow — "approval workflows
// extend authorization, they do not replace it." Fails closed if
// authorization-svc is unreachable or denies.
//
// Idempotent: resubmitting the identical action on an already-resolved
// stage is a no-op (200, no re-publish) — doctrine requirement ("duplicate
// approval submission must not create double-state transition").
//
// Response:
//
//	200 → action applied (or idempotent no-op)
//	400 → missing/invalid field
//	403 → not authorized to approve (from authorization-svc), or wrong approver for this stage
//	404 → workflow not found
//	409 → workflow not PENDING, or stage already resolved to the opposite outcome
//	503 → store or authorization-svc unavailable
func (h *Handler) SubmitAction(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req submitActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.ActorPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "actor_principal_id"})
		return
	}
	if req.Action != "APPROVE" && req.Action != "REJECT" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "action", "message": "must be APPROVE or REJECT"})
		return
	}

	instanceForAuthzCheck, err := h.store.FindWorkflowByID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "SubmitAction")
		return
	}

	if err := h.authz.CheckApprovalAllowed(r.Context(), req.ActorPrincipalID, instanceForAuthzCheck.LegalEntityID); err != nil {
		switch {
		case errors.Is(err, domain.ErrAuthorizationDenied):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "authorization_denied"})
		default:
			h.log.Error("SubmitAction: authorization-svc unavailable — failing closed",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization_service_unavailable"})
		}
		return
	}

	instance, stage, transitioned, err := h.store.SubmitAction(r.Context(), domain.SubmitActionParams{
		WorkflowInstanceID: workflowInstanceID, ActorPrincipalID: req.ActorPrincipalID, Action: req.Action, Rationale: req.Rationale,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrWorkflowNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workflow_not_found"})
		case errors.Is(err, domain.ErrWrongApprover):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong_approver"})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
		default:
			h.log.Error("SubmitAction: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	if transitioned {
		if req.Action == "APPROVE" {
			if pubErr := h.publisher.PublishApprovalGranted(r.Context(), *instance, *stage); pubErr != nil {
				h.log.Error("SubmitAction: failed to publish approval.granted", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		} else {
			if pubErr := h.publisher.PublishApprovalRejected(r.Context(), *instance, *stage); pubErr != nil {
				h.log.Error("SubmitAction: failed to publish approval.rejected", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		}
		if instance.WorkflowStatus == "APPROVED" || instance.WorkflowStatus == "REJECTED" {
			if pubErr := h.publisher.PublishWorkflowCompleted(r.Context(), *instance); pubErr != nil {
				h.log.Error("SubmitAction: failed to publish workflow.completed", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		}
	}

	h.log.Info("workflow action submitted",
		zap.String("workflow_instance_id", workflowInstanceID),
		zap.String("action", req.Action),
		zap.Bool("transitioned", transitioned),
		zap.String("workflow_status", instance.WorkflowStatus),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, workflowResponse{WorkflowInstance: instance, Stages: []*domain.WorkflowStage{stage}})
}

// ── POST /v1/workflows/{id}/escalate ─────────────────────────────────────────

type escalateRequest struct {
	ActorPrincipalID string `json:"actor_principal_id"`
}

// EscalateWorkflow handles POST /v1/workflows/{workflow_instance_id}/escalate.
//
// Response: 200 escalated (or idempotent no-op) / 400 missing field / 404 not found / 409 illegal transition / 503 unavailable.
func (h *Handler) EscalateWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req escalateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.ActorPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "actor_principal_id"})
		return
	}

	instance, transitioned, err := h.store.EscalateWorkflow(r.Context(), workflowInstanceID, req.ActorPrincipalID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "EscalateWorkflow")
		return
	}
	if transitioned {
		if pubErr := h.publisher.PublishWorkflowEscalated(r.Context(), *instance); pubErr != nil {
			h.log.Error("EscalateWorkflow: failed to publish workflow.escalated", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
	}
	writeJSON(w, http.StatusOK, instance)
}

// ── POST /v1/workflows/{id}/cancel ───────────────────────────────────────────

type cancelRequest struct {
	ActorPrincipalID string `json:"actor_principal_id"`
}

// CancelWorkflow handles POST /v1/workflows/{workflow_instance_id}/cancel.
//
// Response: 200 cancelled (or idempotent no-op) / 400 missing field / 404 not found / 409 illegal transition (already terminal) / 503 unavailable.
func (h *Handler) CancelWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req cancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.ActorPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "actor_principal_id"})
		return
	}

	instance, transitioned, err := h.store.CancelWorkflow(r.Context(), workflowInstanceID, req.ActorPrincipalID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "CancelWorkflow")
		return
	}
	if transitioned {
		if pubErr := h.publisher.PublishWorkflowCompleted(r.Context(), *instance); pubErr != nil {
			h.log.Error("CancelWorkflow: failed to publish workflow.completed", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
	}
	writeJSON(w, http.StatusOK, instance)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeStoreErr(w http.ResponseWriter, log *zap.Logger, err error, correlationID, op string) {
	switch {
	case errors.Is(err, domain.ErrWorkflowNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workflow_not_found"})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
	default:
		log.Error(op+": store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
