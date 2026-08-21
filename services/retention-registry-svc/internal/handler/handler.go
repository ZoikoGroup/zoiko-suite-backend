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

	authzpkg "zoiko.io/retention-registry-svc/internal/authz"
	"zoiko.io/retention-registry-svc/internal/domain"
	"zoiko.io/retention-registry-svc/internal/events"
	"zoiko.io/retention-registry-svc/internal/store"
)

const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	RetentionPolicyCreate = "RETENTION_POLICY_CREATE"
	LegalHoldCreate       = "LEGAL_HOLD_CREATE"
	LegalHoldRelease      = "LEGAL_HOLD_RELEASE"
)

type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, tenantID, actionType string) bool {
	scope := platformScopeID
	if tenantID != "" {
		scope = tenantID
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, scope, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/retention-policies", func(r chi.Router) {
		r.Post("/", h.CreateRetentionPolicy)
	})
	r.Route("/v1/legal-holds", func(r chi.Router) {
		r.Post("/", h.CreateLegalHold)
		r.Get("/{id}", h.GetLegalHold)
		r.Post("/{id}/release", h.ReleaseLegalHold)
	})
	r.Get("/v1/retention/resolve", h.Resolve)
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// CreateRetentionPolicy handles POST /v1/retention-policies.
func (h *Handler) CreateRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateRetentionPolicyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RecordClass == "" || req.LegalRegulatoryBasis == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "record_class, legal_regulatory_basis, and effective_from are required")
		return
	}
	if req.MinRetentionDays <= 0 {
		writeError(w, http.StatusBadRequest, "min_retention_days must be positive")
		return
	}
	effectiveFrom, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		writeError(w, http.StatusBadRequest, "effective_from must be RFC3339")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.TenantID, RetentionPolicyCreate) {
		return
	}

	p := &domain.RetentionPolicy{
		RetentionPolicyID:    uuid.NewString(),
		RecordClass:          req.RecordClass,
		JurisdictionCode:     strPtrOrNil(req.JurisdictionCode),
		TenantID:             strPtrOrNil(req.TenantID),
		MinRetentionDays:     req.MinRetentionDays,
		MaxRetentionDays:     req.MaxRetentionDays,
		LegalRegulatoryBasis: req.LegalRegulatoryBasis,
		SourceRightsBasis:    strPtrOrNil(req.SourceRightsBasis),
		PrivacyBasis:         strPtrOrNil(req.PrivacyBasis),
		PolicyStatus:         "ACTIVE",
		EffectiveFrom:        effectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateRetentionPolicy(r.Context(), p); err != nil {
		h.logger.Error("create retention policy failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create retention policy")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "retention_policy.created", EntityID: p.RetentionPolicyID, TenantID: req.TenantID,
		Jurisdiction: req.JurisdictionCode, ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: p,
	})
	writeJSON(w, http.StatusCreated, p)
}

// CreateLegalHold handles POST /v1/legal-holds.
func (h *Handler) CreateLegalHold(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateLegalHoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ScopeDescription == "" || req.Authority == "" {
		writeError(w, http.StatusBadRequest, "scope_description and authority are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.TenantID, LegalHoldCreate) {
		return
	}

	now := time.Now().UTC()
	hld := &domain.LegalHold{
		LegalHoldID:          uuid.NewString(),
		ScopeDescription:     req.ScopeDescription,
		CustodiansObjects:    req.CustodiansObjects,
		Authority:            req.Authority,
		RecordClass:          strPtrOrNil(req.RecordClass),
		TenantID:             strPtrOrNil(req.TenantID),
		EntityRef:            strPtrOrNil(req.EntityRef),
		HoldStatus:           "ACTIVE",
		StartedAt:            now,
		CreatedAt:            now,
		CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateLegalHold(r.Context(), hld); err != nil {
		h.logger.Error("create legal hold failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create legal hold")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "legal_hold.engaged", EntityID: hld.LegalHoldID, TenantID: req.TenantID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: hld,
	})
	writeJSON(w, http.StatusCreated, hld)
}

// GetLegalHold handles GET /v1/legal-holds/{id}.
func (h *Handler) GetLegalHold(w http.ResponseWriter, r *http.Request) {
	hld, err := h.store.FindLegalHoldByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrLegalHoldNotFound) {
			writeError(w, http.StatusNotFound, "legal hold not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get legal hold")
		return
	}
	writeJSON(w, http.StatusOK, hld)
}

// ReleaseLegalHold handles POST /v1/legal-holds/{id}/release — doc7 §J3's
// "release approval". Legal only from ACTIVE.
func (h *Handler) ReleaseLegalHold(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReleaseLegalHoldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ReleaseApprovedByPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "release_approved_by_principal_id is required")
		return
	}

	existing, err := h.store.FindLegalHoldByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrLegalHoldNotFound) {
			writeError(w, http.StatusNotFound, "legal hold not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch legal hold")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	scope := ""
	if existing.TenantID != nil {
		scope = *existing.TenantID
	}
	if !h.authorize(w, r, principalID, scope, LegalHoldRelease) {
		return
	}

	released, err := h.store.ReleaseLegalHold(r.Context(), id, principalID, req.ReleaseApprovedByPrincipalID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrLegalHoldNotFound):
			writeError(w, http.StatusNotFound, "legal hold not found")
		case errors.Is(err, domain.ErrHoldNotActive):
			writeError(w, http.StatusConflict, "legal hold is not currently active")
		default:
			h.logger.Error("release legal hold failed", zap.Error(err))
			writeError(w, http.StatusInternalServerError, "failed to release legal hold")
		}
		return
	}

	tenantForEvent := ""
	if released.TenantID != nil {
		tenantForEvent = *released.TenantID
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "legal_hold.released", EntityID: released.LegalHoldID, TenantID: tenantForEvent,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: released,
	})
	writeJSON(w, http.StatusOK, released)
}

// Resolve handles GET /v1/retention/resolve?record_class=&jurisdiction_code=&tenant_id=&entity_ref=
// — the check every caller makes before deleting/exporting/migrating a
// record. No authz gate: a read-only check every service needs cheaply
// and often, same posture as kill-switch-registry-svc's resolve endpoint.
func (h *Handler) Resolve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	recordClass := q.Get("record_class")
	if recordClass == "" {
		writeError(w, http.StatusBadRequest, "record_class is required")
		return
	}
	jurisdictionCode := strPtrOrNil(q.Get("jurisdiction_code"))
	tenantID := strPtrOrNil(q.Get("tenant_id"))
	entityRef := strPtrOrNil(q.Get("entity_ref"))

	result, err := h.store.Resolve(r.Context(), recordClass, jurisdictionCode, tenantID, entityRef)
	if err != nil {
		h.logger.Error("resolve retention failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to resolve retention")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
