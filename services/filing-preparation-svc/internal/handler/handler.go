package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/filing-preparation-svc/internal/authz"
	"zoiko.io/filing-preparation-svc/internal/domain"
	"zoiko.io/filing-preparation-svc/internal/events"
	"zoiko.io/filing-preparation-svc/internal/evidencereq"
	"zoiko.io/filing-preparation-svc/internal/middleware"
	"zoiko.io/filing-preparation-svc/internal/store"
)

// EvidenceReqClient verifies evidence sufficiency before a filing draft may
// be validated as PREPARED. Implementations must fail closed: a transport
// or service error must never be treated as "sufficient".
type EvidenceReqClient interface {
	Evaluate(ctx context.Context, tenantID, legalEntityID, domainCode, actionType, correlationID, principalID string, artifacts []evidencereq.Artifact) (evidencereq.EvaluateResult, error)
}

const (
	ActionFilingDraftCreate   = "FILING_DRAFT_CREATE"
	ActionFilingDraftUpdate   = "FILING_DRAFT_UPDATE"
	ActionFilingDraftValidate = "FILING_DRAFT_VALIDATE"
	ActionFilingDraftFinalize = "FILING_DRAFT_FINALIZE"
)

type Handler struct {
	store       store.Store
	publisher   events.Publisher
	authz       *authz.Client
	evidenceReq EvidenceReqClient
	logger      *zap.Logger
}

func New(st store.Store, pub events.Publisher, az *authz.Client, er EvidenceReqClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, evidenceReq: er, logger: logger}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/filing-preparation/drafts", func(r chi.Router) {
		r.Post("/", h.Create)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)
		r.Put("/{id}", h.Update)
		r.Post("/{id}/validate", h.Validate)
		r.Post("/{id}/finalize", h.Finalize)
	})
}

// principalID extracts the caller's principal from the X-Principal-Id
// header. If missing, it writes a 401 response and returns ok=false.
func principalID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.Header.Get("X-Principal-Id")
	if id == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return id, true
}

// checkAuthorized calls authorization-svc and writes the appropriate error
// response if the action is denied or the check could not be completed.
// It fails closed: any error is treated as "not authorized".
func (h *Handler) checkAuthorized(w http.ResponseWriter, r *http.Request, principal, legalEntityID, action string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principal, legalEntityID, action); err != nil {
		if errors.Is(err, authz.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization check unavailable")
		}
		return false
	}
	return true
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principal, ok := principalID(w, r)
	if !ok {
		return
	}

	var req domain.CreateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.PeriodKey == "" || req.DueDate == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, period_key, and due_date are required")
		return
	}

	if !h.checkAuthorized(w, r, principal, req.LegalEntityID, ActionFilingDraftCreate) {
		return
	}

	d := &domain.FilingDraft{
		TenantID:            tenantID,
		LegalEntityID:       req.LegalEntityID,
		JurisdictionID:      req.JurisdictionID,
		FilingType:          req.FilingType,
		PeriodKey:           req.PeriodKey,
		DueDate:             req.DueDate,
		PayloadData:         req.PayloadData,
		EvidenceManifestRef: req.EvidenceManifestRef,
		Notes:               req.Notes,
		CreatedBy:           req.CreatedBy,
	}

	if err := h.store.Create(r.Context(), d); err != nil {
		h.logger.Error("create filing draft failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create filing draft")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.draft.created", DraftID: d.DraftID, TenantID: tenantID,
		LegalEntityID: d.LegalEntityID, Jurisdiction: d.JurisdictionID, ActorID: principal,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrDraftNotFound) {
			writeError(w, http.StatusNotFound, "filing draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get filing draft")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	filingType := r.URL.Query().Get("filing_type")
	status := r.URL.Query().Get("status")

	drafts, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, filingType, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list filing drafts")
		return
	}
	if drafts == nil {
		drafts = []domain.FilingDraft{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"filing_drafts": drafts,
		"total":         len(drafts),
	})
}

func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principal, ok := principalID(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrDraftNotFound) {
			writeError(w, http.StatusNotFound, "filing draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing draft")
		return
	}

	if !h.checkAuthorized(w, r, principal, existing.LegalEntityID, ActionFilingDraftUpdate) {
		return
	}

	var req domain.CreateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.PayloadData != "" {
		existing.PayloadData = req.PayloadData
	}
	if req.EvidenceManifestRef != "" {
		existing.EvidenceManifestRef = req.EvidenceManifestRef
	}
	if req.Notes != "" {
		existing.Notes = req.Notes
	}

	if err := h.store.Update(r.Context(), existing); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update filing draft")
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.draft.updated", DraftID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, Jurisdiction: existing.JurisdictionID, ActorID: principal,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: existing,
	})
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) Validate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principal, ok := principalID(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrDraftNotFound) {
			writeError(w, http.StatusNotFound, "filing draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing draft")
		return
	}

	if !h.checkAuthorized(w, r, principal, existing.LegalEntityID, ActionFilingDraftValidate) {
		return
	}

	var req domain.ValidateDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// No finalization path may skip required evidence states
	// (03-microservices.md §8.6). This replaces the previous gate, which
	// only checked whether the CALLER's own RequiredDocumentTypes list was
	// non-empty — a client could always pass by sending an empty list.
	// domain_code is the draft's own filing_type (real data), and the
	// artifact presented is the evidence manifest already attached to the
	// draft, if any.
	// correlationID must NOT default to the draft's own ID:
	// evidence-requirements-svc's Evaluate is deliberately idempotent on
	// correlation_id (a genuine retry must replay, not re-evaluate) — a
	// fixed per-draft fallback would make every retry-after-attaching-
	// evidence permanently replay the FIRST attempt's result (caught live
	// on the sibling board-resolutions-svc fix). Each call without a
	// caller-supplied X-Correlation-ID is a distinct real-world attempt,
	// not a retry, and Validate itself is legitimately called more than
	// once per draft as evidence is assembled.
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		correlationID = "filing-validate-" + uuid.New().String()
	}
	var artifacts []evidencereq.Artifact
	if existing.EvidenceManifestRef != "" {
		artifacts = append(artifacts, evidencereq.Artifact{
			EvidenceType: "EVIDENCE_MANIFEST",
			ReferenceID:  existing.EvidenceManifestRef,
		})
	}
	result, err := h.evidenceReq.Evaluate(r.Context(), tenantID, existing.LegalEntityID,
		existing.FilingType, ActionFilingDraftValidate, correlationID, principal, artifacts)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "evidence-requirements-svc unavailable")
		return
	}

	d, err := h.store.Validate(r.Context(), id, &req, result.Sufficient, result.Reason)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDraftNotFound):
			writeError(w, http.StatusNotFound, "filing draft not found")
		case errors.Is(err, domain.ErrDraftAlreadyFinal):
			writeError(w, http.StatusConflict, "filing draft is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to validate filing draft")
		}
		return
	}

	eventType := "filing.prepared"
	if d.ValidationStatus == domain.StatusBlocked {
		eventType = "filing.blocked"
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: eventType, DraftID: id, TenantID: tenantID,
		LegalEntityID: d.LegalEntityID, Jurisdiction: d.JurisdictionID, ActorID: principal,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) Finalize(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principal, ok := principalID(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrDraftNotFound) {
			writeError(w, http.StatusNotFound, "filing draft not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch filing draft")
		return
	}

	if !h.checkAuthorized(w, r, principal, existing.LegalEntityID, ActionFilingDraftFinalize) {
		return
	}

	var req domain.FinalizeDraftRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	d, err := h.store.Finalize(r.Context(), id, &req)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrDraftNotFound):
			writeError(w, http.StatusNotFound, "filing draft not found")
		case errors.Is(err, domain.ErrValidationBlocked):
			writeError(w, http.StatusUnprocessableEntity, "filing draft is blocked by validation errors")
		case errors.Is(err, domain.ErrDraftAlreadyFinal):
			writeError(w, http.StatusConflict, "filing draft is already finalized")
		default:
			writeError(w, http.StatusInternalServerError, "failed to finalize filing draft")
		}
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "filing.ready.for.submission", DraftID: id, TenantID: tenantID,
		LegalEntityID: d.LegalEntityID, Jurisdiction: d.JurisdictionID, ActorID: principal,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusOK, d)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
