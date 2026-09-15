package handler

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/evidence-requirements-svc/internal/domain"
)

// Action types checked against authorization-svc for PBC request actions.
const (
	actionCreateEvidenceRequest   = "EVIDENCE_REQUEST_CREATE"
	actionManageEvidenceRequest   = "EVIDENCE_REQUEST_MANAGE"
	actionSubmitEvidenceResponse  = "EVIDENCE_REQUEST_SUBMIT_RESPONSE"
	actionEvaluateEvidenceRequest = "EVIDENCE_REQUEST_EVALUATE"
)

// PBCStore is the additional persistence contract AUD-05 needs — kept
// separate from the existing Store interface's own doc comment scope,
// composed onto Handler the same way.
type PBCStore interface {
	CreateEvidenceRequest(ctx context.Context, requestID string, p domain.CreateEvidenceRequestParams) (*domain.EvidenceRequest, bool, error)
	GetEvidenceRequest(ctx context.Context, tenantID, requestID string) (*domain.EvidenceRequest, error)
	ListOpenEvidenceRequests(ctx context.Context, tenantID, legalEntityID string) ([]*domain.EvidenceRequest, error)
	SendEvidenceRequest(ctx context.Context, p domain.SendEvidenceRequestParams) (*domain.EvidenceRequest, error)
	ReassignEvidenceRequest(ctx context.Context, p domain.ReassignEvidenceRequestParams) (*domain.EvidenceRequest, error)
	ExtendDueDate(ctx context.Context, p domain.ExtendDueDateParams) (*domain.EvidenceRequest, error)
	ViewEvidenceRequest(ctx context.Context, p domain.ViewEvidenceRequestParams) (*domain.EvidenceRequest, error)
	SubmitResponse(ctx context.Context, p domain.SubmitResponseParams) (*domain.EvidenceRequestResponse, bool, error)
	RecordScanResult(ctx context.Context, p domain.ScanResultParams) (*domain.EvidenceRequestResponse, error)
	AcknowledgeReceipt(ctx context.Context, p domain.AcknowledgeReceiptParams) (*domain.EvidenceRequestReceipt, error)
	AddRequestNote(ctx context.Context, p domain.AddRequestNoteParams) (*domain.EvidenceRequestNote, error)
	RequestClarification(ctx context.Context, p domain.RequestClarificationParams) (*domain.EvidenceRequest, error)
	MarkSatisfied(ctx context.Context, p domain.MarkSatisfiedParams) (*domain.EvidenceRequest, error)
	CloseEvidenceRequest(ctx context.Context, p domain.CloseEvidenceRequestParams) (*domain.EvidenceRequest, error)
}

func RegisterPBCRoutes(r chi.Router, h *Handler, pbcStore PBCStore) {
	ph := &pbcHandler{Handler: h, store: pbcStore}
	r.Route("/v1/pbc/requests", func(r chi.Router) {
		r.Post("/", ph.CreateRequest)
		r.Get("/", ph.ListOpenRequests)
		r.Get("/{request_id}", ph.GetRequest)
		r.Post("/{request_id}/send", ph.SendRequest)
		r.Post("/{request_id}/reassign", ph.ReassignRequest)
		r.Post("/{request_id}/extend-due-date", ph.ExtendDueDate)
		r.Post("/{request_id}/view", ph.ViewRequest)
		r.Post("/{request_id}/responses", ph.SubmitResponse)
		r.Post("/{request_id}/notes", ph.AddNote)
		r.Post("/{request_id}/clarify", ph.RequestClarification)
		r.Post("/{request_id}/mark-satisfied", ph.MarkSatisfied)
		r.Post("/{request_id}/close", ph.CloseRequest)
	})
	r.Route("/v1/pbc/responses", func(r chi.Router) {
		r.Post("/{response_id}/scan-result", ph.RecordScanResult)
		r.Post("/{response_id}/acknowledge", ph.AcknowledgeReceipt)
	})
}

// pbcHandler embeds *Handler so it can reuse requirePrincipal/requireTenant/
// authorize/writeJSON/writeError without duplicating them.
type pbcHandler struct {
	*Handler
	store PBCStore
}

type createRequestRequest struct {
	LegalEntityID         string    `json:"legal_entity_id"`
	RequirementID         *string   `json:"requirement_id,omitempty"`
	Title                 string    `json:"title"`
	Description           string    `json:"description"`
	AssignedToPrincipalID string    `json:"assigned_to_principal_id"`
	DueAt                 time.Time `json:"due_at"`
	CorrelationID         string    `json:"correlation_id"`
}

func (h *pbcHandler) CreateRequest(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req createRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" || req.AssignedToPrincipalID == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "title, assigned_to_principal_id, and correlation_id are required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCreateEvidenceRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	request, created, err := h.store.CreateEvidenceRequest(r.Context(), uuid.NewString(), domain.CreateEvidenceRequestParams{
		TenantID: tenantID, LegalEntityID: req.LegalEntityID, RequirementID: req.RequirementID, Title: req.Title,
		Description: req.Description, AssignedToPrincipalID: req.AssignedToPrincipalID, DueAt: req.DueAt,
		CreatedByPrincipalID: principalID, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, request)
}

func (h *pbcHandler) GetRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	req, err := h.store.GetEvidenceRequest(r.Context(), tenantID, chi.URLParam(r, "request_id"))
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (h *pbcHandler) ListOpenRequests(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	reqs, err := h.store.ListOpenEvidenceRequests(r.Context(), tenantID, legalEntityID)
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, reqs)
}

// requireManageAuthz fetches the request (to learn its own legal entity)
// and checks the caller holds EVIDENCE_REQUEST_MANAGE for it — shared by
// every admin action below that doesn't already do its own authz check.
func (h *pbcHandler) requireManageAuthz(w http.ResponseWriter, r *http.Request, tenantID, principalID, requestID string) (*domain.EvidenceRequest, bool) {
	req, err := h.store.GetEvidenceRequest(r.Context(), tenantID, requestID)
	if err != nil {
		h.writePBCErr(w, err)
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionManageEvidenceRequest); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return req, true
}

func (h *pbcHandler) SendRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	requestID := chi.URLParam(r, "request_id")
	if _, ok := h.requireManageAuthz(w, r, tenantID, principalID, requestID); !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_correlation_id", "")
		return
	}
	updated, err := h.store.SendEvidenceRequest(r.Context(), domain.SendEvidenceRequestParams{RequestID: requestID, TenantID: tenantID, CorrelationID: correlationID})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type reassignRequestRequest struct {
	NewAssigneePrincipalID string `json:"new_assignee_principal_id"`
}

func (h *pbcHandler) ReassignRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	requestID := chi.URLParam(r, "request_id")
	if _, ok := h.requireManageAuthz(w, r, tenantID, principalID, requestID); !ok {
		return
	}
	var req reassignRequestRequest
	if !decodeJSON(w, r, &req) || req.NewAssigneePrincipalID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "new_assignee_principal_id")
		return
	}
	updated, err := h.store.ReassignEvidenceRequest(r.Context(), domain.ReassignEvidenceRequestParams{RequestID: requestID, TenantID: tenantID, NewAssigneePrincipalID: req.NewAssigneePrincipalID})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type extendDueDateRequest struct {
	NewDueAt time.Time `json:"new_due_at"`
}

func (h *pbcHandler) ExtendDueDate(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	requestID := chi.URLParam(r, "request_id")
	if _, ok := h.requireManageAuthz(w, r, tenantID, principalID, requestID); !ok {
		return
	}
	var req extendDueDateRequest
	if !decodeJSON(w, r, &req) || req.NewDueAt.IsZero() {
		writeError(w, http.StatusBadRequest, "missing_field", "new_due_at")
		return
	}
	updated, err := h.store.ExtendDueDate(r.Context(), domain.ExtendDueDateParams{RequestID: requestID, TenantID: tenantID, NewDueAt: req.NewDueAt})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *pbcHandler) ViewRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	updated, err := h.store.ViewEvidenceRequest(r.Context(), domain.ViewEvidenceRequestParams{RequestID: chi.URLParam(r, "request_id"), TenantID: tenantID})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type submitResponseRequest struct {
	ArtifactDocumentID *string `json:"artifact_document_id,omitempty"`
	CorrelationID      string  `json:"correlation_id"`
}

// SubmitResponse verifies the artifact reference against document-vault-svc
// before accepting it — the same fail-closed pattern already used by
// checkArtifacts elsewhere in this handler package — and always creates a
// NEW response version, never touching a prior one.
func (h *pbcHandler) SubmitResponse(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, actionSubmitEvidenceResponse); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var req submitResponseRequest
	if !decodeJSON(w, r, &req) || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}
	requestID := chi.URLParam(r, "request_id")
	if req.ArtifactDocumentID != nil {
		evReq, err := h.store.GetEvidenceRequest(r.Context(), tenantID, requestID)
		if err != nil {
			h.writePBCErr(w, err)
			return
		}
		if err := h.docs.VerifyDocument(r.Context(), tenantID, evReq.LegalEntityID, *req.ArtifactDocumentID); err != nil {
			h.writeDocumentErr(w, err)
			return
		}
	}
	response, created, err := h.store.SubmitResponse(r.Context(), domain.SubmitResponseParams{
		RequestID: requestID, TenantID: tenantID, SubmittedByPrincipalID: principalID, ArtifactDocumentID: req.ArtifactDocumentID, CorrelationID: req.CorrelationID,
	})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, response)
}

type scanResultRequest struct {
	Result string `json:"result"`
}

// RecordScanResult is the malware-scan callback (AUD-NEG-015): no
// malware-scanning service exists anywhere in this monorepo — this is the
// integration point a future scanner would call. A QUARANTINED result
// leaves the parent request's own status untouched ("preserve request
// state") and is reported here for the audit team to notice.
func (h *pbcHandler) RecordScanResult(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	var req scanResultRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Result != domain.MalwareScanClean && req.Result != domain.MalwareScanQuarantined {
		writeError(w, http.StatusBadRequest, "invalid_field", "result must be CLEAN or QUARANTINED")
		return
	}
	updated, err := h.store.RecordScanResult(r.Context(), domain.ScanResultParams{ResponseID: chi.URLParam(r, "response_id"), TenantID: tenantID, Result: req.Result})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *pbcHandler) AcknowledgeReceipt(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	receipt, err := h.store.AcknowledgeReceipt(r.Context(), domain.AcknowledgeReceiptParams{ResponseID: chi.URLParam(r, "response_id"), TenantID: tenantID, AcknowledgedByPrincipalID: principalID})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, receipt)
}

type addNoteRequest struct {
	Body       string `json:"body"`
	Visibility string `json:"visibility"`
}

// AddNote is the enforcement point for "audit-only notes hidden from
// client contributor" — visibility is stored on the note itself; a
// caller-facing read path (not built here — no client-contributor portal
// exists yet in this monorepo) would filter AUDIT_ONLY notes out.
func (h *pbcHandler) AddNote(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req addNoteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Body == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "body")
		return
	}
	if req.Visibility != domain.NoteVisibilityShared && req.Visibility != domain.NoteVisibilityAuditOnly {
		writeError(w, http.StatusBadRequest, "invalid_field", "visibility must be SHARED or AUDIT_ONLY")
		return
	}
	note, err := h.store.AddRequestNote(r.Context(), domain.AddRequestNoteParams{RequestID: chi.URLParam(r, "request_id"), TenantID: tenantID, AuthorPrincipalID: principalID, Body: req.Body, Visibility: req.Visibility})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, note)
}

type requestClarificationRequest struct {
	Reason string `json:"reason"`
}

func (h *pbcHandler) RequestClarification(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	requestID := chi.URLParam(r, "request_id")
	if _, ok := h.requireManageAuthz(w, r, tenantID, principalID, requestID); !ok {
		return
	}
	var req requestClarificationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	updated, err := h.store.RequestClarification(r.Context(), domain.RequestClarificationParams{RequestID: requestID, TenantID: tenantID, Reason: req.Reason})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// MarkSatisfied is the real enforcement point for AUD-NEG-016 — see
// PgStore.MarkSatisfied's own doc comment.
func (h *pbcHandler) MarkSatisfied(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, tenantID, actionEvaluateEvidenceRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.MarkSatisfied(r.Context(), domain.MarkSatisfiedParams{RequestID: chi.URLParam(r, "request_id"), TenantID: tenantID, ActorPrincipalID: principalID})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *pbcHandler) CloseRequest(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	requestID := chi.URLParam(r, "request_id")
	if _, ok := h.requireManageAuthz(w, r, tenantID, principalID, requestID); !ok {
		return
	}
	updated, err := h.store.CloseEvidenceRequest(r.Context(), domain.CloseEvidenceRequestParams{RequestID: requestID, TenantID: tenantID})
	if err != nil {
		h.writePBCErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *pbcHandler) writeDocumentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDocumentNotFound):
		writeError(w, http.StatusUnprocessableEntity, "document_not_found", "")
	case errors.Is(err, domain.ErrDocumentMismatch):
		writeError(w, http.StatusUnprocessableEntity, "document_mismatch", "")
	default:
		writeError(w, http.StatusServiceUnavailable, "document_service_unavailable", "")
	}
}

func (h *pbcHandler) writePBCErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrEvidenceRequestNotFound):
		writeError(w, http.StatusNotFound, "evidence_request_not_found", "")
	case errors.Is(err, domain.ErrEvidenceRequestInvalidState):
		writeError(w, http.StatusUnprocessableEntity, "invalid_evidence_request_state", "")
	case errors.Is(err, domain.ErrResponseNotFound):
		writeError(w, http.StatusNotFound, "evidence_response_not_found", "")
	case errors.Is(err, domain.ErrSelfEvaluationNotAllowed):
		writeError(w, http.StatusForbidden, "self_evaluation_not_allowed", "")
	case errors.Is(err, domain.ErrArtifactNotScanned):
		writeError(w, http.StatusUnprocessableEntity, "artifact_not_scanned", "")
	default:
		h.log.Error("pbc store error")
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}
