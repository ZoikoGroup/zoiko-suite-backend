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

	authzpkg "zoiko.io/capability-registry-svc/internal/authz"
	"zoiko.io/capability-registry-svc/internal/domain"
	"zoiko.io/capability-registry-svc/internal/events"
	"zoiko.io/capability-registry-svc/internal/store"
)

// platformScopeID is the legal_entity_id passed to authorization-svc — every
// action here is platform administration, never scoped to one tenant. Same
// convention as commercial-account-svc/policy-svc/configuration-feature-flag-svc.
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

const (
	CapabilityCreate            = "CAPABILITY_CREATE"
	MarketReleaseCreate         = "MARKET_RELEASE_CREATE"
	IntegrationCapabilityCreate = "INTEGRATION_CAPABILITY_CREATE"
	IntegrationHealthUpdate     = "INTEGRATION_HEALTH_UPDATE"
	ReleaseStateSet             = "RELEASE_STATE_SET"
	CapabilityClaimCreate       = "CAPABILITY_CLAIM_CREATE"
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

func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, platformScopeID, actionType); err != nil {
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
	r.Route("/v1/capabilities", func(r chi.Router) {
		r.Post("/", h.CreateCapability)
		r.Get("/{id}", h.GetCapability)
		r.Post("/{id}/market-releases", h.CreateMarketRelease)
		r.Post("/{id}/integration-capabilities", h.CreateIntegrationCapability)
		r.Get("/{id}/integration-capabilities", h.ListIntegrationCapabilities)
		r.Post("/{id}/release-state", h.SetReleaseState)
		r.Post("/{id}/claims", h.CreateCapabilityClaim)
		r.Get("/{id}/claims", h.ListCapabilityClaims)
	})
	r.Put("/v1/integration-capabilities/{id}/health", h.UpdateIntegrationHealth)
	r.Get("/v1/capability-resolution/{capabilityCode}", h.ResolveCapability)
}

func (h *Handler) CreateCapability(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCapabilityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CapabilityCode == "" || req.ModuleDomain == "" || req.ExecutionRiskClass == "" {
		writeError(w, http.StatusBadRequest, "capability_code, module_domain, and execution_risk_class are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, CapabilityCreate) {
		return
	}

	version := req.Version
	if version <= 0 {
		version = 1
	}
	c := &domain.Capability{
		CapabilityID:         uuid.NewString(),
		CapabilityCode:       req.CapabilityCode,
		ModuleDomain:         req.ModuleDomain,
		Version:              version,
		ExecutionRiskClass:   req.ExecutionRiskClass,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.Dependencies != "" {
		c.Dependencies = &req.Dependencies
	}
	if err := h.store.CreateCapability(r.Context(), c); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "capability_code already exists")
			return
		}
		h.logger.Error("create capability failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create capability")
		return
	}
	_ = h.publisher.Publish(r.Context(), "capability.created", c.CapabilityID, "", c)
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetCapability(w http.ResponseWriter, r *http.Request) {
	c, err := h.store.GetCapability(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, domain.ErrCapabilityNotFound) {
			writeError(w, http.StatusNotFound, "capability not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get capability")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) CreateMarketRelease(w http.ResponseWriter, r *http.Request) {
	capabilityID := chi.URLParam(r, "id")
	var req domain.CreateMarketReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.MarketCode == "" || req.LegalApprovalStatus == "" || req.State == "" || req.EffectiveFrom == "" {
		writeError(w, http.StatusBadRequest, "market_code, legal_approval_status, state, and effective_from are required")
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
	if !h.authorize(w, r, principalID, MarketReleaseCreate) {
		return
	}

	m := &domain.MarketRelease{
		MarketReleaseID:      uuid.NewString(),
		CapabilityID:         capabilityID,
		MarketCode:           req.MarketCode,
		LegalApprovalStatus:  req.LegalApprovalStatus,
		State:                domain.MarketReleaseState(req.State),
		EffectiveFrom:        effectiveFrom,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.LanguageCode != "" {
		m.LanguageCode = &req.LanguageCode
	}
	if err := h.store.CreateMarketRelease(r.Context(), m); err != nil {
		h.logger.Error("create market release failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create market release")
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) CreateIntegrationCapability(w http.ResponseWriter, r *http.Request) {
	capabilityID := chi.URLParam(r, "id")
	var req domain.CreateIntegrationCapabilityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProviderCode == "" {
		writeError(w, http.StatusBadRequest, "provider_code is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IntegrationCapabilityCreate) {
		return
	}

	i := &domain.IntegrationCapability{
		IntegrationCapabilityID: uuid.NewString(),
		CapabilityID:            capabilityID,
		ProviderCode:            req.ProviderCode,
		Certified:               req.Certified,
		HealthStatus:            req.HealthStatus,
		CreatedAt:               time.Now().UTC(),
		CreatedByPrincipalID:    principalID,
	}
	if err := h.store.CreateIntegrationCapability(r.Context(), i); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "provider already registered for this capability")
			return
		}
		h.logger.Error("create integration capability failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create integration capability")
		return
	}
	writeJSON(w, http.StatusCreated, i)
}

func (h *Handler) ListIntegrationCapabilities(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListIntegrationCapabilitiesByCapability(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list integration capabilities")
		return
	}
	if list == nil {
		list = []domain.IntegrationCapability{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"integration_capabilities": list, "total": len(list)})
}

func (h *Handler) UpdateIntegrationHealth(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.UpdateIntegrationHealthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.HealthStatus == "" {
		writeError(w, http.StatusBadRequest, "health_status is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, IntegrationHealthUpdate) {
		return
	}

	if err := h.store.UpdateIntegrationHealth(r.Context(), id, req.HealthStatus); err != nil {
		if errors.Is(err, domain.ErrIntegrationCapabilityNotFound) {
			writeError(w, http.StatusNotFound, "integration capability not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update integration health")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *Handler) SetReleaseState(w http.ResponseWriter, r *http.Request) {
	capabilityID := chi.URLParam(r, "id")
	var req domain.SetReleaseStateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.State == "" {
		writeError(w, http.StatusBadRequest, "state is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, ReleaseStateSet) {
		return
	}

	rel := &domain.Release{
		ReleaseID:            uuid.NewString(),
		CapabilityID:         capabilityID,
		State:                domain.ReleaseState(req.State),
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.Reason != "" {
		rel.Reason = &req.Reason
	}
	if err := h.store.CreateRelease(r.Context(), rel); err != nil {
		h.logger.Error("set release state failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to set release state")
		return
	}
	_ = h.publisher.Publish(r.Context(), "capability_release.state_changed", capabilityID, "", rel)
	writeJSON(w, http.StatusCreated, rel)
}

func (h *Handler) CreateCapabilityClaim(w http.ResponseWriter, r *http.Request) {
	capabilityID := chi.URLParam(r, "id")
	var req domain.CreateCapabilityClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ClaimText == "" || req.WordingOwnerPrincipalID == "" || req.ApprovedByPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "claim_text, wording_owner_principal_id, and approved_by_principal_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, CapabilityClaimCreate) {
		return
	}

	c := &domain.CapabilityClaim{
		ClaimID:                 uuid.NewString(),
		CapabilityID:            capabilityID,
		ClaimText:               req.ClaimText,
		WordingOwnerPrincipalID: req.WordingOwnerPrincipalID,
		ApprovedByPrincipalID:   req.ApprovedByPrincipalID,
		CreatedAt:               time.Now().UTC(),
		CreatedByPrincipalID:    principalID,
	}
	if req.MarketScope != "" {
		c.MarketScope = &req.MarketScope
	}
	if req.ExpiryReviewDate != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiryReviewDate)
		if err != nil {
			writeError(w, http.StatusBadRequest, "expiry_review_date must be RFC3339")
			return
		}
		c.ExpiryReviewDate = &t
	}
	if err := h.store.CreateCapabilityClaim(r.Context(), c); err != nil {
		h.logger.Error("create capability claim failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create capability claim")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) ListCapabilityClaims(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListClaimsByCapability(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list capability claims")
		return
	}
	if list == nil {
		list = []domain.CapabilityClaim{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"claims": list, "total": len(list)})
}

// ResolveCapability is doc7 §C1's answer service: never let the caller infer
// availability from plan alone. No principal/authz check — this is a read
// path every service (and eventually the UI) needs to hit cheaply and often.
func (h *Handler) ResolveCapability(w http.ResponseWriter, r *http.Request) {
	capabilityCode := chi.URLParam(r, "capabilityCode")
	marketCode := r.URL.Query().Get("market_code")
	resolved, err := h.store.ResolveCapability(r.Context(), capabilityCode, marketCode)
	if err != nil {
		h.logger.Error("resolve capability failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to resolve capability")
		return
	}
	writeJSON(w, http.StatusOK, resolved)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
