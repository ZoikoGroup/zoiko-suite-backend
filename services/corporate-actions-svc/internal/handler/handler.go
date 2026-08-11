package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/corporate-actions-svc/internal/authz"
	"zoiko.io/corporate-actions-svc/internal/domain"
	"zoiko.io/corporate-actions-svc/internal/events"
	"zoiko.io/corporate-actions-svc/internal/evidencereq"
	"zoiko.io/corporate-actions-svc/internal/middleware"
	"zoiko.io/corporate-actions-svc/internal/store"
)

// AuthZClient checks whether a principal is authorized to perform an action
// against authorization-svc. Implementations must fail closed.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// EvidenceReqClient verifies evidence sufficiency before a corporate action
// may be executed. Implementations must fail closed, same doctrine as
// AuthZClient.
type EvidenceReqClient interface {
	EvaluateSufficient(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, artifacts []evidencereq.Artifact) error
}

const (
	actionCorporateActionCreate  = "CORPORATE_ACTION_CREATE"
	actionCorporateActionUpdate  = "CORPORATE_ACTION_UPDATE"
	actionCorporateActionExecute = "CORPORATE_ACTION_EXECUTE"
)

type Handler struct {
	store       store.Store
	publisher   events.Publisher
	authz       AuthZClient
	evidenceReq EvidenceReqClient
	logger      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, er EvidenceReqClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, evidenceReq: er, logger: logger}
}

// requirePrincipal extracts the calling principal from the X-Principal-Id
// header. If missing, it writes a 401 response and returns ok=false.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr maps an authz error to the appropriate HTTP response,
// failing closed on anything that isn't an explicit grant.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "not authorized to perform this action")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// writeEvidenceErr maps an evidence-requirements-svc error to the
// appropriate HTTP response, failing closed on anything ambiguous.
func (h *Handler) writeEvidenceErr(w http.ResponseWriter, err error) {
	if errors.Is(err, evidencereq.ErrEvidenceMissing) {
		writeError(w, http.StatusUnprocessableEntity, "required evidence is missing")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "evidence-requirements-svc unavailable")
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/corporate-actions", func(r chi.Router) {
		r.Post("/", h.CreateAction)
		r.Get("/", h.ListActions)
		r.Get("/{id}", h.GetAction)
		r.Put("/{id}", h.UpdateAction)
		r.Post("/{id}/execute", h.ExecuteAction)
	})
}

func (h *Handler) CreateAction(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateCorporateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.ActionType == "" || req.EffectiveDate == "" {
		writeError(w, http.StatusBadRequest, "title, action_type, and effective_date are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCorporateActionCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	a := &domain.CorporateAction{
		TenantID:        tenantID,
		LegalEntityID:   req.LegalEntityID,
		Title:           req.Title,
		ActionType:      req.ActionType,
		Description:     req.Description,
		ResolutionID:    req.ResolutionID,
		EffectiveDate:   req.EffectiveDate,
		ValuationAmount: req.ValuationAmount,
		Currency:        req.Currency,
		EffectiveFrom:   req.EffectiveFrom,
		CreatedBy:       req.CreatedBy,
	}

	if err := h.store.CreateAction(r.Context(), a); err != nil {
		h.logger.Error("create corporate action failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create corporate action")
		return
	}

	_ = h.publisher.Publish(r.Context(), "corporate_action.created", a.ActionID, tenantID, a)
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) GetAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	a, err := h.store.GetAction(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCorporateActionNotFound) {
			writeError(w, http.StatusNotFound, "corporate action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get corporate action")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) ListActions(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	actionType := r.URL.Query().Get("action_type")
	status := r.URL.Query().Get("status")
	actions, err := h.store.ListActions(r.Context(), legalEntityID, actionType, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list corporate actions")
		return
	}
	if actions == nil {
		actions = []domain.CorporateAction{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"actions": actions, "total": len(actions)})
}

func (h *Handler) UpdateAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetAction(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCorporateActionNotFound) {
			writeError(w, http.StatusNotFound, "corporate action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate action")
		return
	}

	var req domain.UpdateCorporateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionCorporateActionUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.ActionType != "" {
		existing.ActionType = req.ActionType
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.ResolutionID != "" {
		existing.ResolutionID = req.ResolutionID
	}
	if req.EffectiveDate != "" {
		existing.EffectiveDate = req.EffectiveDate
	}
	if req.Status != "" {
		existing.Status = req.Status
	}
	if req.ValuationAmount > 0 {
		existing.ValuationAmount = req.ValuationAmount
	}
	if req.Currency != "" {
		existing.Currency = req.Currency
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateAction(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update corporate action")
		return
	}

	_ = h.publisher.Publish(r.Context(), "corporate_action.updated", id, tenantID, existing)
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) ExecuteAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.ExecuteCorporateActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ExecutedBy == "" {
		writeError(w, http.StatusBadRequest, "executed_by is required")
		return
	}

	existing, err := h.store.GetAction(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCorporateActionNotFound) {
			writeError(w, http.StatusNotFound, "corporate action not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch corporate action")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionCorporateActionExecute); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// a payment batch/corporate action creator may not approve or execute
	// their own submission.
	if existing.CreatedBy == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	// No finalization path may skip required evidence states
	// (03-microservices.md §8.6). domain_code is the action's own type
	// (MERGER/ACQUISITION/SHARE_ISSUANCE/...) — real data already on the
	// record, not an invented dimension.
	//
	// correlationID must NOT default to the action's own ID:
	// evidence-requirements-svc's Evaluate is deliberately idempotent on
	// correlation_id (a genuine retry must replay, not re-evaluate) — a
	// fixed per-action fallback would make every retry-after-attaching-
	// evidence permanently replay the FIRST attempt's result (caught live
	// on the sibling board-resolutions-svc fix). Each call without a
	// caller-supplied X-Correlation-ID is a distinct real-world attempt,
	// not a retry — retries are exactly what the header is for.
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = "action-execute-" + uuid.New().String()
	}
	var artifacts []evidencereq.Artifact
	if req.DocumentVaultID != nil && *req.DocumentVaultID != "" {
		artifacts = append(artifacts, evidencereq.Artifact{
			EvidenceType: "SUPPORTING_DOCUMENT",
			ReferenceID:  *req.DocumentVaultID,
		})
	}
	if err := h.evidenceReq.EvaluateSufficient(r.Context(), tenantID, existing.LegalEntityID,
		string(existing.ActionType), actionCorporateActionExecute, correlationID, principalID, artifacts); err != nil {
		h.writeEvidenceErr(w, err)
		return
	}

	a, err := h.store.ExecuteAction(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrCorporateActionNotFound):
			writeError(w, http.StatusNotFound, "corporate action not found")
		case errors.Is(err, domain.ErrActionAlreadyExecuted):
			writeError(w, http.StatusConflict, "corporate action is already executed")
		default:
			writeError(w, http.StatusInternalServerError, "failed to execute corporate action")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), "corporate_action.executed", id, tenantID, a)
	writeJSON(w, http.StatusOK, a)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
