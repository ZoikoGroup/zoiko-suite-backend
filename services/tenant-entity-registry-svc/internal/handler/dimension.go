package handler

// HTTP surface for REF-08 financial dimensions.
//
//	POST /v1/dimension-types                                   CreateDimensionType (201)
//	GET  /v1/dimension-types                                   the tenant's dimension types
//	GET  /v1/dimension-types/{typeCode}/hierarchy?as_of=&legal_entity_id=            GetHierarchyAsOf
//	GET  /v1/dimension-types/{typeCode}/posting-eligible-values?as_of=&legal_entity_id=   ListPostingEligibleValues
//	POST /v1/dimension-values                                  CreateDimensionValue (201)
//	GET  /v1/dimension-values/{id}?as_of=&history=true         GetDimensionValue
//	POST /v1/dimension-values/{id}/hierarchy                   ReparentDimensionValue (201; PENDING for a controlled type)
//	POST /v1/dimension-values/{id}/hierarchy/{version}:approve approve a pending hierarchy change
//	POST /v1/dimension-values/{id}:activate                    ActivateDimensionValue
//	POST /v1/dimension-values/{id}:deactivate                  DeactivateDimensionValue
//	POST /v1/dimension-values/{id}/source-mappings             MapSourceObject (201, 200 already mapped)
//	GET  /v1/dimensions:resolve?type_code=&code=|&source_domain=&source_object_id=&as_of=    ResolveDimension
//
// Mounted only when the service satisfies DimensionService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// DimensionService is the REF-08 half of the service contract.
type DimensionService interface {
	CreateDimensionType(ctx context.Context, req domain.CreateDimensionTypeRequest) (*domain.DimensionType, error)
	ListDimensionTypes(ctx context.Context) ([]domain.DimensionType, error)
	CreateDimensionValue(ctx context.Context, req domain.CreateDimensionValueRequest) (*registry.DimensionOutcome, error)
	ReparentDimensionValue(ctx context.Context, valueID string, req domain.ReparentDimensionValueRequest) (*registry.DimensionOutcome, error)
	ApproveDimensionHierarchy(ctx context.Context, valueID string, versionNo int, req domain.ApproveDimensionHierarchyRequest) (*registry.DimensionOutcome, error)
	ChangeDimensionStatus(ctx context.Context, valueID, to string, req domain.DimensionStatusRequest) (*registry.DimensionOutcome, error)
	MapSourceObject(ctx context.Context, valueID string, req domain.MapSourceObjectRequest) (*registry.MappingOutcome, error)
	GetDimensionValue(ctx context.Context, valueID string, asOf time.Time, history bool) (*domain.DimensionValueView, error)
	GetHierarchyAsOf(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) (*domain.HierarchyAsOf, error)
	ListPostingEligibleValues(ctx context.Context, typeCode, legalEntityID string, asOf time.Time) ([]registry.EligibleDimensionValue, error)
	ResolveDimension(ctx context.Context, q domain.ResolveDimensionQuery) (*domain.DimensionResolution, error)
}

func registerDimensionRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(DimensionService); !ok {
		return
	}
	r.Post("/dimension-types", h.CreateDimensionType)
	r.Get("/dimension-types", h.ListDimensionTypes)
	r.Get("/dimension-types/{typeCode}/hierarchy", h.GetHierarchyAsOf)
	r.Get("/dimension-types/{typeCode}/posting-eligible-values", h.ListPostingEligibleValues)
	r.Post("/dimension-values", h.CreateDimensionValue)
	r.Get("/dimension-values/{valueID}", h.GetDimensionValue)
	r.Post("/dimension-values/{valueID}/hierarchy", h.ReparentDimensionValue)
	r.Post("/dimension-values/{valueID}/hierarchy/{versionNo}:approve", h.ApproveDimensionHierarchy)
	r.Post("/dimension-values/{valueID}:activate", h.dimensionStatus(domain.DimActive))
	r.Post("/dimension-values/{valueID}:deactivate", h.dimensionStatus(domain.DimInactive))
	r.Post("/dimension-values/{valueID}/source-mappings", h.MapSourceObject)
	r.Get("/dimensions:resolve", h.ResolveDimension)
}

func (h *Handler) dimensions() DimensionService {
	ds, _ := h.svc.(DimensionService)
	return ds
}

// CreateDimensionType creates a dimension type.
func (h *Handler) CreateDimensionType(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDimensionTypeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.dimensions().CreateDimensionType(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ListDimensionTypes lists the tenant's dimension types.
func (h *Handler) ListDimensionTypes(w http.ResponseWriter, r *http.Request) {
	out, err := h.dimensions().ListDimensionTypes(r.Context())
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// CreateDimensionValue opens a DRAFT value.
func (h *Handler) CreateDimensionValue(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDimensionValueRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.dimensions().CreateDimensionValue(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ReparentDimensionValue proposes the next hierarchy version.
func (h *Handler) ReparentDimensionValue(w http.ResponseWriter, r *http.Request) {
	var req domain.ReparentDimensionValueRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.dimensions().ReparentDimensionValue(r.Context(), chi.URLParam(r, "valueID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ApproveDimensionHierarchy approves a pending hierarchy change.
func (h *Handler) ApproveDimensionHierarchy(w http.ResponseWriter, r *http.Request) {
	no, err := strconv.Atoi(chi.URLParam(r, "versionNo"))
	if err != nil || no < 1 {
		writeErrJSON(w, r, http.StatusBadRequest, "the hierarchy version must be a positive integer", correlationID(r))
		return
	}
	var req domain.ApproveDimensionHierarchyRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.dimensions().ApproveDimensionHierarchy(r.Context(), chi.URLParam(r, "valueID"), no, req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// dimensionStatus is one named status command: the route, not the body, names the target status.
func (h *Handler) dimensionStatus(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.DimensionStatusRequest
		if !decode(w, r, &req) {
			return
		}
		if req.CorrelationID == "" {
			req.CorrelationID = correlationID(r)
		}
		out, err := h.dimensions().ChangeDimensionStatus(r.Context(), chi.URLParam(r, "valueID"), to, req)
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// MapSourceObject maps an operational source object to a value.
func (h *Handler) MapSourceObject(w http.ResponseWriter, r *http.Request) {
	var req domain.MapSourceObjectRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.dimensions().MapSourceObject(r.Context(), chi.URLParam(r, "valueID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.NoOp {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// GetDimensionValue returns the value as it was on as_of.
func (h *Handler) GetDimensionValue(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.dimensions().GetDimensionValue(r.Context(), chi.URLParam(r, "valueID"), asOf, r.URL.Query().Get("history") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetHierarchyAsOf returns a type's hierarchy as it stood on as_of.
func (h *Handler) GetHierarchyAsOf(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.dimensions().GetHierarchyAsOf(r.Context(), chi.URLParam(r, "typeCode"), r.URL.Query().Get("legal_entity_id"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListPostingEligibleValues lists the values that could take a new posting on as_of.
func (h *Handler) ListPostingEligibleValues(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.dimensions().ListPostingEligibleValues(r.Context(), chi.URLParam(r, "typeCode"), r.URL.Query().Get("legal_entity_id"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ResolveDimension finds a value by code or by source object.
func (h *Handler) ResolveDimension(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := h.dimensions().ResolveDimension(r.Context(), domain.ResolveDimensionQuery{TypeCode: q.Get("type_code"), Code: q.Get("code"),
		LegalEntityID: q.Get("legal_entity_id"), SourceDomain: q.Get("source_domain"), SourceObjectID: q.Get("source_object_id"), AsOf: asOf})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// The production service offers dimensions; if it ever stops, the routes would silently vanish.
var _ DimensionService = (*registry.Service)(nil)
