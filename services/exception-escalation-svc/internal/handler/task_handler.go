package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/exception-escalation-svc/internal/domain"
	"zoiko.io/exception-escalation-svc/internal/events"
	"zoiko.io/exception-escalation-svc/internal/middleware"
	"zoiko.io/exception-escalation-svc/internal/store"
)

const (
	actionTaskCreate = "TASK_CREATE"
	actionTaskManage = "TASK_MANAGE"
	actionTaskClose  = "TASK_CLOSE"
)

// taskHandler embeds *Handler so it reuses requirePrincipal/writeAuthzErr
// without duplicating them — same composition pattern used for AUD-08's
// findingHandler.
type taskHandler struct {
	*Handler
	store store.TaskStore
}

func RegisterTaskRoutes(r chi.Router, h *Handler, taskStore store.TaskStore) {
	th := &taskHandler{Handler: h, store: taskStore}
	r.Route("/v1/cases", func(r chi.Router) {
		r.Post("/", th.CreateCase)
		r.Get("/{case_id}", th.GetCase)
		r.Post("/{case_id}/close", th.CloseCase)
	})
	r.Route("/v1/tasks", func(r chi.Router) {
		r.Post("/", th.CreateTask)
		r.Get("/{task_id}", th.GetTask)
		r.Post("/{task_id}/assign", th.AssignTask)
		r.Post("/{task_id}/start", th.StartTask)
		r.Get("/{task_id}/history", th.GetTaskHistory)
	})
}

type createCaseRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	CaseType      string `json:"case_type"`
	Purpose       string `json:"purpose,omitempty"`
}

// CreateCase — BIZ-05's own CreateCase command (a gap-fill; see
// internal/domain/task.go's own doc comment on why).
func (h *taskHandler) CreateCase(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.CaseType == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and case_type are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionTaskCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c, err := h.store.CreateCase(r.Context(), domain.CreateCaseParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID,
		CaseType: req.CaseType, Purpose: req.Purpose, CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// GetCase — BIZ-05's own GetCase query.
func (h *taskHandler) GetCase(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.GetCase(r.Context(), chi.URLParam(r, "case_id"))
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type closeCaseRequest struct {
	ClosureReason string `json:"closure_reason,omitempty"`
}

// CloseCase — BIZ-05's own CloseCase command. Fetched (read-only) BEFORE
// authorization and BEFORE the mutation, same fetch-then-authorize-then-
// mutate discipline as every handler in this platform.
func (h *taskHandler) CloseCase(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req closeCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	caseID := chi.URLParam(r, "case_id")
	existing, err := h.store.GetCase(r.Context(), caseID)
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTaskClose); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	closed, err := h.store.CloseCase(r.Context(), domain.CloseCaseParams{
		CaseID: caseID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID, ClosureReason: req.ClosureReason,
	})
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "case.closed", CaseID: caseID, TenantID: closed.TenantID, LegalEntityID: closed.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: closed,
	}); err != nil {
		h.logger.Warn("failed to publish case.closed event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, closed)
}

type createTaskRequest struct {
	LegalEntityID    string              `json:"legal_entity_id"`
	CaseID           string              `json:"case_id,omitempty"`
	TaskType         string              `json:"task_type"`
	Priority         domain.TaskPriority `json:"priority,omitempty"`
	BusinessTrigger  string              `json:"business_trigger,omitempty"`
	LinkedObjectType string              `json:"linked_object_type"`
	LinkedObjectID   string              `json:"linked_object_id"`
	RequiredEvidence string              `json:"required_evidence,omitempty"`
	SLADeadline      *time.Time          `json:"sla_deadline,omitempty"`
	AssignedToRole   string              `json:"assigned_to_role,omitempty"`
	AssignedToUser   string              `json:"assigned_to_user,omitempty"`
}

// CreateTask — BIZ-05's own CreateTask command. Lands NEW.
func (h *taskHandler) CreateTask(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req createTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.TaskType == "" || req.LinkedObjectType == "" || req.LinkedObjectID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, task_type, linked_object_type, and linked_object_id are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionTaskCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	t, err := h.store.CreateTask(r.Context(), domain.CreateTaskParams{
		TenantID: middleware.GetTenantID(r.Context()), LegalEntityID: req.LegalEntityID, CaseID: req.CaseID,
		TaskType: req.TaskType, Priority: req.Priority, BusinessTrigger: req.BusinessTrigger,
		LinkedObjectType: req.LinkedObjectType, LinkedObjectID: req.LinkedObjectID, RequiredEvidence: req.RequiredEvidence,
		SLADeadline: req.SLADeadline, AssignedToRole: req.AssignedToRole, AssignedToUser: req.AssignedToUser,
		CreatedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "task.created", CaseID: t.TaskID, TenantID: t.TenantID, LegalEntityID: t.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: t,
	}); err != nil {
		h.logger.Warn("failed to publish task.created event", zap.Error(err))
	}
	writeJSON(w, http.StatusCreated, t)
}

// GetTask — BIZ-05's own GetTask query.
func (h *taskHandler) GetTask(w http.ResponseWriter, r *http.Request) {
	t, err := h.store.GetTask(r.Context(), chi.URLParam(r, "task_id"))
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type assignTaskRequest struct {
	AssignedToRole string `json:"assigned_to_role,omitempty"`
	AssignedToUser string `json:"assigned_to_user,omitempty"`
}

// AssignTask — BIZ-05's own Assign command.
func (h *taskHandler) AssignTask(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req assignTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AssignedToRole == "" && req.AssignedToUser == "" {
		writeError(w, http.StatusBadRequest, "assigned_to_role or assigned_to_user is required")
		return
	}
	taskID := chi.URLParam(r, "task_id")
	existing, err := h.store.GetTask(r.Context(), taskID)
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTaskManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	t, err := h.store.AssignTask(r.Context(), domain.AssignTaskParams{
		TaskID: taskID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID,
		AssignedToRole: req.AssignedToRole, AssignedToUser: req.AssignedToUser,
	})
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if err := h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "task.assigned", CaseID: taskID, TenantID: t.TenantID, LegalEntityID: t.LegalEntityID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: t,
	}); err != nil {
		h.logger.Warn("failed to publish task.assigned event", zap.Error(err))
	}
	writeJSON(w, http.StatusOK, t)
}

// StartTask — BIZ-05's own Start command.
func (h *taskHandler) StartTask(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	taskID := chi.URLParam(r, "task_id")
	existing, err := h.store.GetTask(r.Context(), taskID)
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTaskManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	t, err := h.store.StartTask(r.Context(), domain.StartTaskParams{TaskID: taskID, TenantID: middleware.GetTenantID(r.Context()), ActorPrincipalID: principalID})
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// GetTaskHistory — BIZ-05's own GetHistory query.
func (h *taskHandler) GetTaskHistory(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "task_id")
	history, err := h.store.GetTaskHistory(r.Context(), middleware.GetTenantID(r.Context()), taskID)
	if err != nil {
		h.writeTaskErr(w, err)
		return
	}
	if history == nil {
		history = []domain.TaskTransition{}
	}
	writeJSON(w, http.StatusOK, history)
}

func (h *taskHandler) writeTaskErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrCaseNotFound):
		writeError(w, http.StatusNotFound, "case not found")
	case errors.Is(err, domain.ErrCaseAlreadyClosedWC):
		writeError(w, http.StatusConflict, "case is already closed")
	case errors.Is(err, domain.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "task not found")
	case errors.Is(err, domain.ErrTaskInvalidState):
		writeError(w, http.StatusConflict, "task is not in a state that permits this action")
	default:
		h.logger.Error("task store error", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "task store unavailable")
	}
}
