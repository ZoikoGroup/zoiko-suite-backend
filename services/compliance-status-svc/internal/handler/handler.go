package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/compliance-status-svc/internal/authz"
	"zoiko.io/compliance-status-svc/internal/domain"
	"zoiko.io/compliance-status-svc/internal/middleware"
	"zoiko.io/compliance-status-svc/internal/store"
)

// Action types used when checking authorization for write operations against
// authorization-svc. Every write action must be checked before it happens and
// must fail closed (see internal/authz/client.go).
const (
	actionComplianceStatusEvaluate = "COMPLIANCE_STATUS_EVALUATE"
	actionComplianceGapCreate      = "COMPLIANCE_GAP_CREATE"
	actionComplianceGapResolve     = "COMPLIANCE_GAP_RESOLVE"

	// Read actions: GET /, GET /{id} and GET /gaps used to be served to any
	// caller in the tenant without an authorization decision.
	actionComplianceStatusRead = "COMPLIANCE_STATUS_READ"
	actionComplianceGapRead    = "COMPLIANCE_GAP_READ"
)

// Stable error codes for refusals added by the actor and segregation-of-
// duties rules.
const (
	codeActorMismatch       = "actor_mismatch"
	codeSegregationOfDuties = "segregation_of_duties"
)

// AuthZClient checks whether a principal is authorized to perform an action
// against a legal entity. Implementations must fail closed.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Handler serves the compliance-status API. Events are not published from
// here: every write emits its event through the store's transactional
// outbox, delivered by internal/outbox.Relay.
type Handler struct {
	store  store.Store
	authz  AuthZClient
	logger *zap.Logger
}

func New(st store.Store, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, logger: logger}
}

// eventMeta is the acting principal and correlation of a write.
func eventMeta(r *http.Request, principalID string) store.EventMeta {
	return store.EventMeta{ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID")}
}

// authorizeRead requires a principal (401) holding action for scope (403 on
// deny, 503 when authorization-svc is unreachable). Pattern:
// purchase-request-svc authorizeRead.
func (h *Handler) authorizeRead(w http.ResponseWriter, r *http.Request, scope, action string) bool {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, action); err != nil {
		h.writeAuthzErr(w, err)
		return false
	}
	return true
}

// listScope is the legal entity a list is filtered to, or the tenant when it
// spans entities (purchase-request-svc ListRequests).
func listScope(r *http.Request) string {
	if le := r.URL.Query().Get("legal_entity_id"); le != "" {
		return le
	}
	return middleware.GetTenantID(r.Context())
}

// requirePrincipal extracts the calling principal from the X-Principal-Id
// header, responding with 401 and returning ok=false if absent.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "principal identity missing")
		return "", false
	}
	return principalID, true
}

// writeAuthzErr responds appropriately for an error returned by AuthZClient.CheckAllowed:
// explicit denial maps to 403, anything else (including unavailability) fails
// closed as 503.
func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "action not authorized")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/compliance-status", func(r chi.Router) {
		r.Post("/evaluate", h.Evaluate)
		r.Get("/", h.List)
		r.Get("/{id}", h.GetByID)

		r.Post("/gaps", h.CreateGap)
		r.Get("/gaps", h.ListGaps)
		r.Post("/gaps/{id}/resolve", h.ResolveGap)
	})
}

func (h *Handler) Evaluate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.EvaluateComplianceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id and jurisdiction_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionComplianceStatusEvaluate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// The actor is the authenticated principal: an absent created_by is
	// filled from it, a different one is refused.
	if req.CreatedBy != "" && req.CreatedBy != principalID {
		writeCoded(w, http.StatusForbidden, codeActorMismatch, "created_by must be the authenticated principal (X-Principal-Id)")
		return
	}

	c := &domain.ComplianceHealth{
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		JurisdictionID:       req.JurisdictionID,
		DomainName:           req.DomainName,
		TotalObligations:     req.TotalObligations,
		FulfilledObligations: req.FulfilledObligations,
		PendingObligations:   req.PendingObligations,
		OverdueObligations:   req.OverdueObligations,
		OpenExceptions:       req.OpenExceptions,
		Notes:                req.Notes,
		EffectiveFrom:        req.EffectiveFrom,
		CreatedBy:            principalID,
	}

	if err := h.store.Evaluate(r.Context(), c, eventMeta(r, principalID)); err != nil {
		h.logger.Error("evaluate compliance status failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to evaluate compliance status")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) GetByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	c, err := h.store.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrStatusRecordNotFound) {
			writeError(w, http.StatusNotFound, "compliance status record not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get compliance status record")
		return
	}
	if !h.authorizeRead(w, r, c.LegalEntityID, actionComplianceStatusRead) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	domainName := r.URL.Query().Get("domain_name")
	status := r.URL.Query().Get("status")

	if !h.authorizeRead(w, r, listScope(r), actionComplianceStatusRead) {
		return
	}

	records, err := h.store.List(r.Context(), legalEntityID, jurisdictionID, domainName, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list compliance status records")
		return
	}
	if records == nil {
		records = []domain.ComplianceHealth{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"compliance_status_records": records,
		"total":                     len(records),
	})
}

func (h *Handler) CreateGap(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.CreateGapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.LegalEntityID == "" || req.JurisdictionID == "" || req.DomainName == "" || req.GapType == "" {
		writeError(w, http.StatusBadRequest, "legal_entity_id, jurisdiction_id, domain_name, and gap_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionComplianceGapCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	g := &domain.ComplianceGap{
		TenantID:        tenantID,
		LegalEntityID:   req.LegalEntityID,
		JurisdictionID:  req.JurisdictionID,
		DomainName:      req.DomainName,
		GapType:         req.GapType,
		Severity:        req.Severity,
		SourceReference: req.SourceReference,
		Description:     req.Description,
		RemediationPlan: req.RemediationPlan,
	}

	if err := h.store.CreateGap(r.Context(), g, eventMeta(r, principalID)); err != nil {
		h.logger.Error("create compliance gap failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to log compliance gap")
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

func (h *Handler) ListGaps(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	domainName := r.URL.Query().Get("domain_name")
	severity := r.URL.Query().Get("severity")
	status := r.URL.Query().Get("status")

	if !h.authorizeRead(w, r, listScope(r), actionComplianceGapRead) {
		return
	}

	gaps, err := h.store.ListGaps(r.Context(), legalEntityID, domainName, severity, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list compliance gaps")
		return
	}
	if gaps == nil {
		gaps = []domain.ComplianceGap{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"compliance_gaps": gaps,
		"total":           len(gaps),
	})
}

func (h *Handler) ResolveGap(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.ResolveGapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetGapByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrGapNotFound) {
			writeError(w, http.StatusNotFound, "compliance gap not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to resolve compliance gap")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionComplianceGapResolve); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// Segregation of duties: the principal who logged the gap may not
	// resolve it. Re-checked by the store under the row lock.
	if existing.CreatedBy != nil && *existing.CreatedBy == principalID {
		writeCoded(w, http.StatusForbidden, codeSegregationOfDuties, domain.ErrSegregationOfDuties.Error())
		return
	}

	g, err := h.store.ResolveGap(r.Context(), id, &req, eventMeta(r, principalID))
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrGapNotFound):
			writeError(w, http.StatusNotFound, "compliance gap not found")
		case errors.Is(err, domain.ErrGapAlreadyResolved):
			writeError(w, http.StatusConflict, "compliance gap is already resolved")
		case errors.Is(err, domain.ErrSegregationOfDuties):
			writeCoded(w, http.StatusForbidden, codeSegregationOfDuties, domain.ErrSegregationOfDuties.Error())
		default:
			h.logger.Error("resolve compliance gap failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to resolve compliance gap")
		}
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeCoded is writeError plus a stable machine-readable code (the "error"
// field keeps its existing human-readable meaning).
func writeCoded(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}
