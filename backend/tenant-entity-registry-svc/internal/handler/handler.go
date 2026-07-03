// Package handler exposes the REST API for tenant-entity-registry-svc via chi.
//
// All mutating endpoints:
//   1. Extract the IdentityContextEnvelope JWT from Authorization header.
//   2. Extract X-Correlation-ID for propagation and evidence.
//   3. Delegate to the registry.Service — which calls AuthorizationClient first.
//   4. Map service errors to HTTP status codes.
//
// Read-only endpoints do not require the Authorization header (the service
// itself still validates entity existence but does not call AuthZ for reads).
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

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// svc is the interface slice of service methods consumed by the handler.
// Using the concrete *registry.Service here keeps wiring simple; an interface
// can be extracted later if multiple service implementations are needed.
type svc interface {
	// Tenant
	ProvisionTenant(ctx context.Context, envelopeJWT string, req domain.ProvisionTenantRequest, correlationID string) (*domain.Tenant, error)
	GetTenant(ctx context.Context, tenantID string) (*domain.Tenant, error)
	TransitionTenantLifecycle(ctx context.Context, envelopeJWT, tenantID string, req domain.TransitionTenantLifecycleRequest) error

	// Entity
	CreateEntity(ctx context.Context, envelopeJWT string, req domain.CreateEntityRequest) (*domain.LegalEntity, error)
	GetEntity(ctx context.Context, legalEntityID string) (*domain.LegalEntity, error)
	ListEntities(ctx context.Context, tenantID string) ([]*domain.LegalEntity, error)
	UpdateEntity(ctx context.Context, envelopeJWT, legalEntityID string, req domain.UpdateEntityRequest) (*domain.LegalEntity, error)
	GetEntityStatus(ctx context.Context, legalEntityID string) (*domain.EntityStatusResponse, error)
	TransitionEntityStatus(ctx context.Context, envelopeJWT, legalEntityID string, req domain.TransitionEntityStatusRequest) error

	// Hierarchy
	CreateHierarchy(ctx context.Context, envelopeJWT string, req domain.CreateHierarchyRequest) (*domain.EntityHierarchy, error)
	EndDateHierarchy(ctx context.Context, envelopeJWT, hierarchyID string, endDate time.Time, correlationID string) error
	ListHierarchies(ctx context.Context, legalEntityID string) ([]*domain.EntityHierarchy, error)

	// Jurisdiction assignment
	AssignJurisdiction(ctx context.Context, envelopeJWT, legalEntityID string, req domain.AssignJurisdictionRequest) (*domain.EntityJurisdictionAssignment, error)
	ListJurisdictions(ctx context.Context, legalEntityID string) ([]*domain.EntityJurisdictionAssignment, error)
	EndDateJurisdictionAssignment(ctx context.Context, envelopeJWT, assignmentID string, endDate time.Time, correlationID string) error

	// Residency policy
	CreateResidencyPolicy(ctx context.Context, envelopeJWT string, req domain.CreateResidencyPolicyRequest) (*domain.DataResidencyPolicy, error)
	GetResidencyPolicy(ctx context.Context, policyID string) (*domain.DataResidencyPolicy, error)

	// ResidencyRegion — read-only (IaC-managed)
	GetResidencyRegion(ctx context.Context, regionID string) (*domain.ResidencyRegion, error)
	ListResidencyRegions(ctx context.Context) ([]*domain.ResidencyRegion, error)

	// TaxIdentityBundle
	CreateTaxIdentityBundle(ctx context.Context, envelopeJWT, legalEntityID string, req domain.CreateTaxIdentityBundleRequest) (*domain.TaxIdentityBundle, error)
	GetTaxIdentityBundle(ctx context.Context, bundleID string) (*domain.TaxIdentityBundle, error)
	ListTaxIdentityBundles(ctx context.Context, legalEntityID string) ([]*domain.TaxIdentityBundle, error)
	TransitionTaxIdentityBundleStatus(ctx context.Context, envelopeJWT, bundleID string, req domain.TransitionTaxIdentityBundleStatusRequest) error
}

// Handler holds all HTTP handler methods.
type Handler struct {
	svc svc
	log *zap.Logger
}

// New constructs a Handler.
func New(s *registry.Service, log *zap.Logger) *Handler {
	return &Handler{svc: s, log: log}
}

// RegisterRoutes mounts all endpoints under a chi Router.
// Route convention: /v1/<resource> — URI-versioned per API-first doctrine.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1", func(r chi.Router) {
		// ── Tenants ─────────────────────────────────────────────────────────
		r.Post("/tenants", h.ProvisionTenant)
		r.Get("/tenants/{tenantID}", h.GetTenant)
		r.Post("/tenants/{tenantID}/lifecycle", h.TransitionTenantLifecycle)

		// ── Entities ─────────────────────────────────────────────────────────
		r.Get("/tenants/{tenantID}/entities", h.ListEntities)
		r.Post("/entities", h.CreateEntity)
		r.Get("/entities/{entityID}", h.GetEntity)
		r.Patch("/entities/{entityID}", h.UpdateEntity)
		// Status probe — renamed GET /v1/entities/{entityID}/status per approved answers.
		r.Get("/entities/{entityID}/status", h.GetEntityStatus)
		r.Post("/entities/{entityID}/status", h.TransitionEntityStatus)

		// ── Hierarchies ──────────────────────────────────────────────────────
		r.Post("/entity-hierarchies", h.CreateHierarchy)
		r.Get("/entities/{entityID}/hierarchies", h.ListHierarchies)
		r.Delete("/entity-hierarchies/{hierarchyID}", h.EndDateHierarchy)

		// ── Jurisdiction assignments ─────────────────────────────────────────
		r.Post("/entities/{entityID}/jurisdictions", h.AssignJurisdiction)
		r.Get("/entities/{entityID}/jurisdictions", h.ListJurisdictions)
		r.Delete("/entity-jurisdictions/{assignmentID}", h.EndDateJurisdictionAssignment)

		// ── Data Residency Policies ──────────────────────────────────────────
		r.Post("/residency-policies", h.CreateResidencyPolicy)
		r.Get("/residency-policies/{policyID}", h.GetResidencyPolicy)

		// ── Residency Regions — read-only (IaC-managed, per Q1 resolution) ───
		r.Get("/residency-regions", h.ListResidencyRegions)
		r.Get("/residency-regions/{regionID}", h.GetResidencyRegion)

		// ── TaxIdentityBundles ───────────────────────────────────────────────
		r.Post("/entities/{entityID}/tax-identity-bundles", h.CreateTaxIdentityBundle)
		r.Get("/entities/{entityID}/tax-identity-bundles", h.ListTaxIdentityBundles)
		r.Get("/tax-identity-bundles/{bundleID}", h.GetTaxIdentityBundle)
		r.Post("/tax-identity-bundles/{bundleID}/status", h.TransitionTaxIdentityBundleStatus)
	})
}

// ── Tenant handlers ─────────────────────────────────────────────────────────

func (h *Handler) ProvisionTenant(w http.ResponseWriter, r *http.Request) {
	var req domain.ProvisionTenantRequest
	if !decode(w, r, &req) {
		return
	}
	t, err := h.svc.ProvisionTenant(r.Context(), bearerToken(r), req, correlationID(r))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handler) GetTenant(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.GetTenant(r.Context(), chi.URLParam(r, "tenantID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handler) TransitionTenantLifecycle(w http.ResponseWriter, r *http.Request) {
	var req domain.TransitionTenantLifecycleRequest
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.TransitionTenantLifecycle(r.Context(), bearerToken(r), chi.URLParam(r, "tenantID"), req); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Entity handlers ──────────────────────────────────────────────────────────

func (h *Handler) CreateEntity(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEntityRequest
	if !decode(w, r, &req) {
		return
	}
	e, err := h.svc.CreateEntity(r.Context(), bearerToken(r), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

func (h *Handler) GetEntity(w http.ResponseWriter, r *http.Request) {
	e, err := h.svc.GetEntity(r.Context(), chi.URLParam(r, "entityID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

func (h *Handler) ListEntities(w http.ResponseWriter, r *http.Request) {
	entities, err := h.svc.ListEntities(r.Context(), chi.URLParam(r, "tenantID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entities)
}

func (h *Handler) UpdateEntity(w http.ResponseWriter, r *http.Request) {
	var req domain.UpdateEntityRequest
	if !decode(w, r, &req) {
		return
	}
	e, err := h.svc.UpdateEntity(r.Context(), bearerToken(r), chi.URLParam(r, "entityID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// GetEntityStatus — GET /v1/entities/{entityID}/status
// Lightweight status probe. Renamed per approved answers; no full entity payload returned.
func (h *Handler) GetEntityStatus(w http.ResponseWriter, r *http.Request) {
	resp, err := h.svc.GetEntityStatus(r.Context(), chi.URLParam(r, "entityID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// TransitionEntityStatus — POST /v1/entities/{entityID}/status
// Applies a state-machine transition and publishes entity.status.changed.
func (h *Handler) TransitionEntityStatus(w http.ResponseWriter, r *http.Request) {
	var req domain.TransitionEntityStatusRequest
	if !decode(w, r, &req) {
		return
	}
	req.CorrelationID = correlationID(r)
	if err := h.svc.TransitionEntityStatus(r.Context(), bearerToken(r), chi.URLParam(r, "entityID"), req); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Hierarchy handlers ───────────────────────────────────────────────────────

func (h *Handler) CreateHierarchy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateHierarchyRequest
	if !decode(w, r, &req) {
		return
	}
	hier, err := h.svc.CreateHierarchy(r.Context(), bearerToken(r), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, hier)
}

func (h *Handler) ListHierarchies(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListHierarchies(r.Context(), chi.URLParam(r, "entityID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// EndDateHierarchy — DELETE is used as "end-date" semantics (no hard-delete per doctrine).
// The effective_to date is read from the query parameter `end_date` (RFC3339).
func (h *Handler) EndDateHierarchy(w http.ResponseWriter, r *http.Request) {
	endDate, ok := parseEndDate(w, r)
	if !ok {
		return
	}
	if err := h.svc.EndDateHierarchy(r.Context(), bearerToken(r), chi.URLParam(r, "hierarchyID"), endDate, correlationID(r)); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Jurisdiction assignment handlers ────────────────────────────────────────

func (h *Handler) AssignJurisdiction(w http.ResponseWriter, r *http.Request) {
	var req domain.AssignJurisdictionRequest
	if !decode(w, r, &req) {
		return
	}
	a, err := h.svc.AssignJurisdiction(r.Context(), bearerToken(r), chi.URLParam(r, "entityID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Handler) ListJurisdictions(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListJurisdictions(r.Context(), chi.URLParam(r, "entityID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) EndDateJurisdictionAssignment(w http.ResponseWriter, r *http.Request) {
	endDate, ok := parseEndDate(w, r)
	if !ok {
		return
	}
	if err := h.svc.EndDateJurisdictionAssignment(r.Context(), bearerToken(r), chi.URLParam(r, "assignmentID"), endDate, correlationID(r)); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Residency policy handlers ────────────────────────────────────────────────

func (h *Handler) CreateResidencyPolicy(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateResidencyPolicyRequest
	if !decode(w, r, &req) {
		return
	}
	p, err := h.svc.CreateResidencyPolicy(r.Context(), bearerToken(r), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) GetResidencyPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.GetResidencyPolicy(r.Context(), chi.URLParam(r, "policyID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ── ResidencyRegion handlers — read-only (IaC-managed) ──────────────────────

func (h *Handler) ListResidencyRegions(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListResidencyRegions(r.Context())
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) GetResidencyRegion(w http.ResponseWriter, r *http.Request) {
	region, err := h.svc.GetResidencyRegion(r.Context(), chi.URLParam(r, "regionID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, region)
}

// ── TaxIdentityBundle handlers ───────────────────────────────────────────────

func (h *Handler) CreateTaxIdentityBundle(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateTaxIdentityBundleRequest
	if !decode(w, r, &req) {
		return
	}
	b, err := h.svc.CreateTaxIdentityBundle(r.Context(), bearerToken(r), chi.URLParam(r, "entityID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, b)
}

func (h *Handler) GetTaxIdentityBundle(w http.ResponseWriter, r *http.Request) {
	b, err := h.svc.GetTaxIdentityBundle(r.Context(), chi.URLParam(r, "bundleID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) ListTaxIdentityBundles(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListTaxIdentityBundles(r.Context(), chi.URLParam(r, "entityID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) TransitionTaxIdentityBundleStatus(w http.ResponseWriter, r *http.Request) {
	var req domain.TransitionTaxIdentityBundleStatusRequest
	if !decode(w, r, &req) {
		return
	}
	if err := h.svc.TransitionTaxIdentityBundleStatus(r.Context(), bearerToken(r), chi.URLParam(r, "bundleID"), req); err != nil {
		h.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Error mapping ────────────────────────────────────────────────────────────

func (h *Handler) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	corrID := correlationID(r)
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeErrJSON(w, http.StatusNotFound, "not found", corrID)
	case errors.Is(err, registry.ErrInvalidTransition):
		writeErrJSON(w, http.StatusUnprocessableEntity, err.Error(), corrID)
	case errors.Is(err, registry.ErrUnauthorized):
		writeErrJSON(w, http.StatusForbidden, "forbidden", corrID)
	case errors.Is(err, registry.ErrServiceUnavailable):
		writeErrJSON(w, http.StatusServiceUnavailable, "upstream service unavailable — request rejected", corrID)
	case errors.Is(err, registry.ErrInvalidInput):
		writeErrJSON(w, http.StatusBadRequest, err.Error(), corrID)
	case errors.Is(err, registry.ErrConflict):
		writeErrJSON(w, http.StatusConflict, err.Error(), corrID)
	default:
		h.log.Error("unhandled service error", zap.Error(err), zap.String("correlation_id", corrID))
		writeErrJSON(w, http.StatusInternalServerError, "internal server error", corrID)
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

type errorResponse struct {
	Error         string `json:"error"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErrJSON(w http.ResponseWriter, status int, msg, corrID string) {
	writeJSON(w, status, errorResponse{Error: msg, CorrelationID: corrID})
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErrJSON(w, http.StatusBadRequest, "invalid request body", correlationID(r))
		return false
	}
	return true
}

func bearerToken(r *http.Request) string {
	val := r.Header.Get("Authorization")
	val = strings.TrimPrefix(val, "Bearer ")
	return val
}

func correlationID(r *http.Request) string {
	if id := r.Header.Get("X-Correlation-ID"); id != "" {
		return id
	}
	return ""
}

func parseEndDate(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	raw := r.URL.Query().Get("end_date")
	if raw == "" {
		writeErrJSON(w, http.StatusBadRequest, "end_date query parameter is required (RFC3339)", correlationID(r))
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeErrJSON(w, http.StatusBadRequest, "end_date must be RFC3339 format", correlationID(r))
		return time.Time{}, false
	}
	return t, true
}
