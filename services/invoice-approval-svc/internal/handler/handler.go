package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/invoice-approval-svc/internal/clients"
	"zoiko.io/invoice-approval-svc/internal/domain"
	svcmiddleware "zoiko.io/invoice-approval-svc/internal/middleware"
)

type Store interface {
	CreateRequest(ctx context.Context, req *domain.InvoiceApprovalRequest) error
	GetRequest(ctx context.Context, id string) (*domain.InvoiceApprovalRequest, error)
	ListRequests(ctx context.Context, legalEntityID, invoiceID, status string) ([]domain.InvoiceApprovalRequest, error)
	AddDecisionAndUpdateStatus(ctx context.Context, decision *domain.ApprovalDecision, newStatus string, newStep int) error
	GetDecisionsByRequest(ctx context.Context, requestID string) ([]domain.ApprovalDecision, error)
}

type Publisher interface {
	PublishApprovalStarted(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest)
	PublishApproved(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest)
	PublishRejected(ctx context.Context, correlationID string, req domain.InvoiceApprovalRequest, reason string)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type DomainClients interface {
	FetchInvoice(ctx context.Context, tenantID, invoiceID string) (*clients.APInvoice, error)
	StartWorkflowInstance(ctx context.Context, tenantID, invoiceID, principalID string) (string, error)
}

const (
	actionApprovalInitiate = "INVOICE_APPROVAL_INITIATE"
	actionApprovalView     = "INVOICE_APPROVAL_VIEW"
	actionApprovalDecide   = "INVOICE_APPROVAL_DECIDE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	clients   DomainClients
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, clients DomainClients, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		clients:   clients,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/invoice-approvals", func(r chi.Router) {
		r.Post("/", h.CreateRequest)
		r.Get("/", h.ListRequests)
		r.Get("/{id}", h.GetRequest)
		r.Post("/{id}/decide", h.SubmitDecision)
	})
}

// ── POST /v1/invoice-approvals ────────────────────────────────────────────────────

func (h *Handler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.InvoiceID == "" || req.LegalEntityID == "" || req.InvoiceAmount <= 0 || req.CurrencyCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "invoice_id, legal_entity_id, invoice_amount (> 0), currency_code are required")
		return
	}

	if req.TotalSteps <= 0 {
		req.TotalSteps = 1
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionApprovalInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)

	// Fetch invoice details from AP client (optional validation pass)
	_, err := h.clients.FetchInvoice(r.Context(), tenantID, req.InvoiceID)
	if err != nil {
		h.log.Warn("AP invoice fetch failed — proceeding with provided request fields", zap.Error(err))
	}

	// Trigger workflow instance
	wfInstanceID, err := h.clients.StartWorkflowInstance(r.Context(), tenantID, req.InvoiceID, principalID)
	if err != nil {
		wfInstanceID = uuid.NewString()
	}

	now := time.Now().UTC()
	appReq := &domain.InvoiceApprovalRequest{
		ApprovalRequestID:    uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		InvoiceID:            req.InvoiceID,
		WorkflowInstanceID:   wfInstanceID,
		InvoiceAmount:        req.InvoiceAmount,
		CurrencyCode:         req.CurrencyCode,
		Status:               "PENDING",
		CurrentStep:          1,
		TotalSteps:           req.TotalSteps,
		CreatedByPrincipalID: principalID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	if err := h.store.CreateRequest(r.Context(), appReq); err != nil {
		h.log.Error("failed to create invoice approval request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	h.publisher.PublishApprovalStarted(r.Context(), correlationID, *appReq)

	writeJSON(w, http.StatusCreated, appReq)
}

// ── GET /v1/invoice-approvals ─────────────────────────────────────────────────────

func (h *Handler) ListRequests(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	invoiceID := r.URL.Query().Get("invoice_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionApprovalView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListRequests(r.Context(), legalEntityID, invoiceID, status)
	if err != nil {
		h.log.Error("failed to list invoice approval requests", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if list == nil {
		list = []domain.InvoiceApprovalRequest{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/invoice-approvals/{id} ────────────────────────────────────────────────

func (h *Handler) GetRequest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	appReq, err := h.store.GetRequest(r.Context(), id)
	if errors.Is(err, domain.ErrRequestNotFound) {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch invoice approval request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, appReq.LegalEntityID, actionApprovalView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	decisions, err := h.store.GetDecisionsByRequest(r.Context(), id)
	if err != nil {
		h.log.Error("failed to fetch approval decisions", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if decisions == nil {
		decisions = []domain.ApprovalDecision{}
	}

	writeJSON(w, http.StatusOK, domain.ApprovalDetailResponse{
		Request:   *appReq,
		Decisions: decisions,
	})
}

// ── POST /v1/invoice-approvals/{id}/decide ────────────────────────────────────────

func (h *Handler) SubmitDecision(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.SubmitDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.Decision != "APPROVED" && req.Decision != "REJECTED" {
		writeError(w, http.StatusBadRequest, "invalid_decision", "decision must be APPROVED or REJECTED")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	appReq, err := h.store.GetRequest(r.Context(), id)
	if errors.Is(err, domain.ErrRequestNotFound) {
		writeError(w, http.StatusNotFound, "request_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to fetch invoice approval request", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if appReq.Status != "PENDING" {
		writeError(w, http.StatusConflict, "already_finalized", string(domain.ErrRequestAlreadyFinalized))
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, appReq.LegalEntityID, actionApprovalDecide); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	correlationID := getCorrelationID(r)

	now := time.Now().UTC()
	decision := &domain.ApprovalDecision{
		ApprovalDecisionID:   uuid.NewString(),
		TenantID:             tenantID,
		ApprovalRequestID:    id,
		StepNumber:           appReq.CurrentStep,
		DecidedByPrincipalID: principalID,
		Decision:             req.Decision,
		DecisionReason:       req.DecisionReason,
		DecidedAt:            now,
	}

	newStatus := "PENDING"
	newStep := appReq.CurrentStep

	if req.Decision == "REJECTED" {
		newStatus = "REJECTED"
	} else if req.Decision == "APPROVED" {
		if appReq.CurrentStep >= appReq.TotalSteps {
			newStatus = "APPROVED"
		} else {
			newStep++
		}
	}

	if err := h.store.AddDecisionAndUpdateStatus(r.Context(), decision, newStatus, newStep); err != nil {
		h.log.Error("failed to save decision and update status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	appReq.Status = newStatus
	appReq.CurrentStep = newStep
	appReq.UpdatedAt = now

	if newStatus == "APPROVED" {
		h.publisher.PublishApproved(r.Context(), correlationID, *appReq)
	} else if newStatus == "REJECTED" {
		h.publisher.PublishRejected(r.Context(), correlationID, *appReq, req.DecisionReason)
	}

	writeJSON(w, http.StatusOK, appReq)
}

// ── Helpers ──────────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}