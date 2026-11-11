package handler

// HTTP surface for REF-09 units of measure.
//
//	POST /v1/quantity-dimensions                                     create a quantity dimension with its base unit (201)
//	GET  /v1/quantity-dimensions                                     the quantity dimensions of the tenant
//	GET  /v1/quantity-dimensions/{dimensionCode}/units               ListUnitsByDimension
//	POST /v1/units                                                   CreateUnit (201, DRAFT)
//	GET  /v1/units/{unitCode}?as_of=&history=true                    GetUnit
//	POST /v1/units/{unitCode}/conversions                            first conversion of a unit (201; PENDING unless STANDARD)
//	POST /v1/units/{unitCode}/conversions:supersede                  SupersedeConversion (201; PENDING unless STANDARD)
//	GET  /v1/units/{unitCode}/conversions/{version}                  GetConversionVersion
//	POST /v1/units/{unitCode}/conversions/{version}:approve          ApproveConversion
//	POST /v1/units/{unitCode}:retire                                 RetireUnit
//	GET  /v1/units:convert?quantity=&from=&to=&as_of=                ConvertQuantity
//
// Mounted only when the service satisfies UnitService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// UnitService is the REF-09 half of the service contract.
type UnitService interface {
	CreateQuantityDimension(ctx context.Context, req domain.CreateQuantityDimensionRequest) (*registry.DimensionWithBase, error)
	ListQuantityDimensions(ctx context.Context) ([]domain.QuantityDimension, error)
	CreateUnit(ctx context.Context, req domain.CreateUnitRequest) (*registry.UnitOutcome, error)
	ProposeConversion(ctx context.Context, unitCode string, supersede bool, req domain.ProposeConversionRequest) (*registry.UnitOutcome, error)
	ApproveConversion(ctx context.Context, unitCode string, versionNo int, req domain.ApproveConversionRequest) (*registry.UnitOutcome, error)
	RetireUnit(ctx context.Context, unitCode string, req domain.RetireUnitRequest) (*registry.UnitOutcome, error)
	GetUnit(ctx context.Context, unitCode string, asOf time.Time, history bool) (*domain.UnitView, error)
	GetConversionVersion(ctx context.Context, unitCode string, versionNo int) (*domain.ConversionVersion, error)
	ListUnitsByDimension(ctx context.Context, dimensionCode string) ([]domain.Unit, error)
	ConvertQuantity(ctx context.Context, q domain.ConvertQuantityQuery) (*domain.ConversionResult, error)
}

func registerUnitRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(UnitService); !ok {
		return
	}
	r.Post("/quantity-dimensions", h.CreateQuantityDimension)
	r.Get("/quantity-dimensions", h.ListQuantityDimensions)
	r.Get("/quantity-dimensions/{dimensionCode}/units", h.ListUnitsByDimension)
	r.Post("/units", h.CreateUnit)
	r.Get("/units:convert", h.ConvertQuantity)
	r.Get("/units/{unitCode}", h.GetUnit)
	r.Post("/units/{unitCode}/conversions", h.proposeConversion(false))
	r.Post("/units/{unitCode}/conversions:supersede", h.proposeConversion(true))
	r.Get("/units/{unitCode}/conversions/{versionNo}", h.GetConversionVersion)
	r.Post("/units/{unitCode}/conversions/{versionNo}:approve", h.ApproveConversion)
	r.Post("/units/{unitCode}:retire", h.RetireUnit)
}

func (h *Handler) units() UnitService {
	us, _ := h.svc.(UnitService)
	return us
}

// CreateQuantityDimension creates a quantity dimension with its base unit.
func (h *Handler) CreateQuantityDimension(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateQuantityDimensionRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.units().CreateQuantityDimension(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ListQuantityDimensions lists the quantity dimensions of the tenant.
func (h *Handler) ListQuantityDimensions(w http.ResponseWriter, r *http.Request) {
	out, err := h.units().ListQuantityDimensions(r.Context())
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ListUnitsByDimension lists the units of a dimension.
func (h *Handler) ListUnitsByDimension(w http.ResponseWriter, r *http.Request) {
	out, err := h.units().ListUnitsByDimension(r.Context(), chi.URLParam(r, "dimensionCode"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// CreateUnit opens a DRAFT unit.
func (h *Handler) CreateUnit(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateUnitRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.units().CreateUnit(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// proposeConversion is one named conversion command: the route says whether it replaces the active conversion.
func (h *Handler) proposeConversion(supersede bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.ProposeConversionRequest
		if !decode(w, r, &req) {
			return
		}
		if req.CorrelationID == "" {
			req.CorrelationID = correlationID(r)
		}
		out, err := h.units().ProposeConversion(r.Context(), chi.URLParam(r, "unitCode"), supersede, req)
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, out)
	}
}

// ApproveConversion approves a pending conversion.
func (h *Handler) ApproveConversion(w http.ResponseWriter, r *http.Request) {
	no, err := strconv.Atoi(chi.URLParam(r, "versionNo"))
	if err != nil || no < 1 {
		writeErrJSON(w, r, http.StatusBadRequest, "the conversion version must be a positive integer", correlationID(r))
		return
	}
	var req domain.ApproveConversionRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.units().ApproveConversion(r.Context(), chi.URLParam(r, "unitCode"), no, req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RetireUnit retires a unit.
func (h *Handler) RetireUnit(w http.ResponseWriter, r *http.Request) {
	var req domain.RetireUnitRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.units().RetireUnit(r.Context(), chi.URLParam(r, "unitCode"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetUnit returns the unit as it was on as_of.
func (h *Handler) GetUnit(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.units().GetUnit(r.Context(), chi.URLParam(r, "unitCode"), asOf, r.URL.Query().Get("history") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetConversionVersion returns one conversion version.
func (h *Handler) GetConversionVersion(w http.ResponseWriter, r *http.Request) {
	no, err := strconv.Atoi(chi.URLParam(r, "versionNo"))
	if err != nil || no < 1 {
		writeErrJSON(w, r, http.StatusBadRequest, "the conversion version must be a positive integer", correlationID(r))
		return
	}
	out, err := h.units().GetConversionVersion(r.Context(), chi.URLParam(r, "unitCode"), no)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ConvertQuantity converts a quantity between two units of one dimension.
func (h *Handler) ConvertQuantity(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := h.units().ConvertQuantity(r.Context(), domain.ConvertQuantityQuery{Quantity: q.Get("quantity"), From: q.Get("from"), To: q.Get("to"), AsOf: asOf})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// The production service offers units; if it ever stops, the routes would silently vanish.
var _ UnitService = (*registry.Service)(nil)
