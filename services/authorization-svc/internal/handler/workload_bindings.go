package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
)

// Workload bindings (ZS-IAM-001 §16, Scenarios A18/A19; Governance audit
// N-09, 13 Oct 2026).
//
// A binding is a tenant's statement that a named workload (a service acting
// on its own behalf) may act in it: for one audience, and only the actions it
// lists. 000017 created the table and the decision engine read it, but
// nothing could write it, so every WORKLOAD-typed request was denied as
// unbound and allowed_actions was never consulted. These routes are the
// administration; evaluateDecision enforces the actions.
//
// Gated by iam.workload.manage in the caller's tenant. That action is not yet
// in access-control-svc's permission registry, so it is granted through this
// service's own admin API until access-control registers it.

// PermissionWorkloadManage authorizes the workload-binding routes.
const PermissionWorkloadManage = "iam.workload.manage"

type workloadBindingStore interface {
	SaveWorkloadBinding(ctx context.Context, wb domain.WorkloadBinding, event domain.OutboxMessage) (*domain.WorkloadBinding, error)
	SetWorkloadBindingActive(ctx context.Context, workloadID, tenantID string, active bool, event domain.OutboxMessage) (*domain.WorkloadBinding, error)
	ListWorkloadBindings(ctx context.Context, tenantID string) ([]domain.WorkloadBinding, error)
}

type saveWorkloadBindingRequest struct {
	WorkloadID      string   `json:"workload_id"`
	TenantID        string   `json:"tenant_id,omitempty"`
	AllowedAudience string   `json:"allowed_audience"`
	AllowedActions  []string `json:"allowed_actions"`
	ReasonCode      string   `json:"reason_code"`
	Purpose         string   `json:"purpose"`
}

func (h *Handler) workloadStore(w http.ResponseWriter) (workloadBindingStore, bool) {
	s, ok := h.store.(workloadBindingStore)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
	return s, ok
}

func workloadEvent(r *http.Request, eventType, tenantID, actor string, wb *domain.WorkloadBinding, reason string) (domain.OutboxMessage, error) {
	payload := map[string]any{
		"workload_id":      wb.WorkloadID,
		"tenant_id":        tenantID,
		"allowed_audience": wb.AllowedAudience,
		"allowed_actions":  wb.AllowedActions,
		"active":           wb.ActiveFlag,
		"changed_by":       actor,
		"command_reason":   reason,
		"changed_at":       time.Now().UTC(),
	}
	return events.NewEvent(eventType, r.Header.Get("X-Correlation-ID"), tenantID, actor, wb.WorkloadID, payload)
}

// SaveWorkloadBinding handles POST /v1/admin/workload-bindings: create or
// replace the binding of a workload in the caller's tenant.
//
// Response: 200 the binding / 400 missing or wildcard field, or no reason /
// 401 / 403 / 503.
func (h *Handler) SaveWorkloadBinding(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, PermissionWorkloadManage) {
		return
	}
	var req saveWorkloadBindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}
	req.WorkloadID = strings.TrimSpace(req.WorkloadID)
	req.AllowedAudience = strings.TrimSpace(req.AllowedAudience)
	switch {
	case req.WorkloadID == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "workload_id"})
		return
	case req.AllowedAudience == "" || strings.Contains(req.AllowedAudience, "*"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "allowed_audience",
			"message": "a binding names the one audience its tokens are issued for; a wildcard is not accepted (ZS-IAM-001 §16, A18)"})
		return
	case len(req.AllowedActions) == 0:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "allowed_actions",
			"message": "a binding lists the actions the workload may take"})
		return
	}
	for _, a := range req.AllowedActions {
		if strings.TrimSpace(a) == "" || domain.IsWildcardAction(a) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "allowed_actions",
				"message": "allowed_actions names actions; a wildcard or empty entry is not accepted"})
			return
		}
	}
	reason := commandFields{ReasonCode: req.ReasonCode, Purpose: req.Purpose}.reason()
	if !h.applyReason(w, &r, reason) {
		return
	}
	store, ok := h.workloadStore(w)
	if !ok {
		return
	}
	wb := domain.WorkloadBinding{WorkloadID: req.WorkloadID, TenantID: tenantScope, AllowedAudience: req.AllowedAudience,
		AllowedActions: req.AllowedActions, ActiveFlag: true}
	event, err := workloadEvent(r, "iam.workload_binding.saved", tenantScope, principalID, &wb, reason)
	if err == nil {
		var saved *domain.WorkloadBinding
		if saved, err = store.SaveWorkloadBinding(r.Context(), wb, event); err == nil {
			writeJSON(w, http.StatusOK, saved)
			return
		}
	}
	h.log.Error("SaveWorkloadBinding failed", zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
}

// ListWorkloadBindings handles GET /v1/admin/workload-bindings.
func (h *Handler) ListWorkloadBindings(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, PermissionWorkloadManage) {
		return
	}
	store, ok := h.workloadStore(w)
	if !ok {
		return
	}
	list, err := store.ListWorkloadBindings(r.Context(), tenantScope)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workload_bindings": list})
}

// DeactivateWorkloadBinding / ReactivateWorkloadBinding handle
// POST /v1/admin/workload-bindings/{workload_id}/deactivate and /reactivate.
// A deactivated binding denies the workload everything in the tenant
// (WORKLOAD_BINDING_INACTIVE) at the next decision: the binding read is not
// cached.
func (h *Handler) DeactivateWorkloadBinding(w http.ResponseWriter, r *http.Request) {
	h.setWorkloadBindingActive(w, r, false)
}

func (h *Handler) ReactivateWorkloadBinding(w http.ResponseWriter, r *http.Request) {
	h.setWorkloadBindingActive(w, r, true)
}

func (h *Handler) setWorkloadBindingActive(w http.ResponseWriter, r *http.Request, active bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, PermissionWorkloadManage) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	store, ok := h.workloadStore(w)
	if !ok {
		return
	}
	_, _, reason := domain.AuditFrom(r.Context())
	workloadID := chi.URLParam(r, "workload_id")
	eventType := "iam.workload_binding.deactivated"
	if active {
		eventType = "iam.workload_binding.reactivated"
	}
	event, err := workloadEvent(r, eventType, tenantScope, principalID,
		&domain.WorkloadBinding{WorkloadID: workloadID, ActiveFlag: active}, reason)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	out, err := store.SetWorkloadBindingActive(r.Context(), workloadID, tenantScope, active, event)
	switch {
	case errors.Is(err, domain.ErrWorkloadBindingNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workload_binding_not_found"})
	case err != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	default:
		writeJSON(w, http.StatusOK, out)
	}
}
