package handler

// HTTP surface for REF-03 exchange rates.
//
//	POST /v1/fx/sources                          register and approve a rate source (201)
//	GET  /v1/fx/sources                          the tenant's sources
//	POST /v1/fx/sources/{id}:withdraw            withdraw a source: no new rates from it
//	GET  /v1/fx/sources/{id}/rates?from=&to=     ListRatesBySource
//	POST /v1/fx/observations                     IngestFXObservation (201, 200 replay)
//	POST /v1/fx/observations/{id}:supersede      SupersedeRateObservation
//	POST /v1/fx/rate-sets                        ApproveFXRateSet (201)
//	POST /v1/fx/rate-sets/{id}:close             CloseRateSet
//	GET  /v1/fx/rate-sets/{id}                   GetRateSet
//	GET  /v1/fx/rates?base=&quote=&rate_type=&market_date=      GetRate: the approved direct rate, else 404 FX_RATE_MISSING
//	GET  /v1/fx/rates:history?...&known_at=      GetHistoricalRate: as it was known at that instant
//	GET  /v1/fx/rates:explain?...&via=           ExplainDerivedCrossRate: direct, inverse or cross, with its components
//
// Mounted only when the service satisfies FXService, which *registry.Service does.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// FXService is the REF-03 half of the service contract.
type FXService interface {
	CreateFXSource(ctx context.Context, req domain.CreateFXSourceRequest) (*domain.FXSource, error)
	WithdrawFXSource(ctx context.Context, sourceID string, req domain.WithdrawFXSourceRequest) (*domain.FXSource, error)
	ListFXSources(ctx context.Context) ([]domain.FXSource, error)
	IngestFXObservation(ctx context.Context, req domain.IngestFXObservationRequest) (*registry.FXIngestOutcome, error)
	SupersedeFXObservation(ctx context.Context, observationID string, req domain.SupersedeFXObservationRequest) (*registry.FXIngestOutcome, error)
	ApproveFXRateSet(ctx context.Context, req domain.ApproveFXRateSetRequest) (*domain.FXRateSet, error)
	CloseFXRateSet(ctx context.Context, setID string, req domain.CloseFXRateSetRequest) (*domain.FXRateSet, error)
	GetFXRateSet(ctx context.Context, setID string) (*domain.FXRateSet, error)
	ListFXRatesBySource(ctx context.Context, sourceID string, from, to time.Time, limit int, after string) ([]domain.FXObservation, string, error)
	GetFXRate(ctx context.Context, q registry.FXRateQuery) (*domain.FXRate, error)
	GetFXHistoricalRate(ctx context.Context, q registry.FXRateQuery) (*domain.FXRate, error)
	ExplainDerivedCrossRate(ctx context.Context, q registry.FXRateQuery) (*domain.FXRate, error)
}

func registerFXRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(FXService); !ok {
		return
	}
	r.Post("/fx/sources", h.CreateFXSource)
	r.Get("/fx/sources", h.ListFXSources)
	r.Post("/fx/sources/{sourceID}:withdraw", h.WithdrawFXSource)
	r.Get("/fx/sources/{sourceID}/rates", h.ListFXRatesBySource)
	r.Post("/fx/observations", h.IngestFXObservation)
	r.Post("/fx/observations/{observationID}:supersede", h.SupersedeFXObservation)
	r.Post("/fx/rate-sets", h.ApproveFXRateSet)
	r.Post("/fx/rate-sets/{setID}:close", h.CloseFXRateSet)
	r.Get("/fx/rate-sets/{setID}", h.GetFXRateSet)
	r.Get("/fx/rates", h.GetFXRate)
	r.Get("/fx/rates:history", h.GetFXHistoricalRate)
	r.Get("/fx/rates:explain", h.ExplainDerivedCrossRate)
}

func (h *Handler) fx() FXService {
	fs, _ := h.svc.(FXService)
	return fs
}

// CreateFXSource registers a source.
func (h *Handler) CreateFXSource(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateFXSourceRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.fx().CreateFXSource(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ListFXSources lists the tenant's sources.
func (h *Handler) ListFXSources(w http.ResponseWriter, r *http.Request) {
	out, err := h.fx().ListFXSources(r.Context())
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// WithdrawFXSource withdraws a source.
func (h *Handler) WithdrawFXSource(w http.ResponseWriter, r *http.Request) {
	var req domain.WithdrawFXSourceRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.fx().WithdrawFXSource(r.Context(), chi.URLParam(r, "sourceID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// IngestFXObservation records one observation.
func (h *Handler) IngestFXObservation(w http.ResponseWriter, r *http.Request) {
	var req domain.IngestFXObservationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.fx().IngestFXObservation(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// SupersedeFXObservation replaces an observation not yet in a set.
func (h *Handler) SupersedeFXObservation(w http.ResponseWriter, r *http.Request) {
	var req domain.SupersedeFXObservationRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.fx().SupersedeFXObservation(r.Context(), chi.URLParam(r, "observationID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// ApproveFXRateSet approves a set of observations.
func (h *Handler) ApproveFXRateSet(w http.ResponseWriter, r *http.Request) {
	var req domain.ApproveFXRateSetRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.fx().ApproveFXRateSet(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// CloseFXRateSet certifies a set.
func (h *Handler) CloseFXRateSet(w http.ResponseWriter, r *http.Request) {
	var req domain.CloseFXRateSetRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.fx().CloseFXRateSet(r.Context(), chi.URLParam(r, "setID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetFXRateSet reads a set with its members.
func (h *Handler) GetFXRateSet(w http.ResponseWriter, r *http.Request) {
	out, err := h.fx().GetFXRateSet(r.Context(), chi.URLParam(r, "setID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListFXRatesBySource lists a source's observations.
func (h *Handler) ListFXRatesBySource(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var from, to time.Time
	for key, dst := range map[string]*time.Time{"from": &from, "to": &to} {
		if raw := strings.TrimSpace(q.Get(key)); raw != "" {
			d, err := domain.ParseDate(raw)
			if err != nil {
				writeErrJSON(w, r, http.StatusBadRequest, key+" must be a date, YYYY-MM-DD", correlationID(r))
				return
			}
			*dst = d
		}
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeErrJSON(w, r, http.StatusBadRequest, "limit must be an integer from 1 to 200", correlationID(r))
			return
		}
		limit = n
	}
	out, next, err := h.fx().ListFXRatesBySource(r.Context(), chi.URLParam(r, "sourceID"), from, to, limit, q.Get("cursor"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// fxQuery reads the pair, type, market date, set, pivot and known_at of a rate question.
func fxQuery(w http.ResponseWriter, r *http.Request) (registry.FXRateQuery, bool) {
	q := r.URL.Query()
	out := registry.FXRateQuery{Base: strings.ToUpper(q.Get("base")), Quote: strings.ToUpper(q.Get("quote")),
		Type: domain.FXRateType(q.Get("rate_type")), SetName: q.Get("set_name"), Via: strings.ToUpper(q.Get("via"))}
	if raw := strings.TrimSpace(q.Get("market_date")); raw != "" {
		d, err := domain.ParseDate(raw)
		if err != nil {
			writeErrJSON(w, r, http.StatusBadRequest, "market_date must be a date, YYYY-MM-DD", correlationID(r))
			return out, false
		}
		out.MarketDate = d
	}
	if raw := strings.TrimSpace(q.Get("known_at")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErrJSON(w, r, http.StatusBadRequest, "known_at must be an RFC3339 timestamp", correlationID(r))
			return out, false
		}
		out.KnownAt = t.UTC()
	}
	return out, true
}

// GetFXRate returns the approved direct rate, or 404 FX_RATE_MISSING.
func (h *Handler) GetFXRate(w http.ResponseWriter, r *http.Request) {
	q, ok := fxQuery(w, r)
	if !ok {
		return
	}
	out, err := h.fx().GetFXRate(r.Context(), q)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetFXHistoricalRate returns the rate as it was known at known_at.
func (h *Handler) GetFXHistoricalRate(w http.ResponseWriter, r *http.Request) {
	q, ok := fxQuery(w, r)
	if !ok {
		return
	}
	out, err := h.fx().GetFXHistoricalRate(r.Context(), q)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ExplainDerivedCrossRate derives a rate and shows its components.
func (h *Handler) ExplainDerivedCrossRate(w http.ResponseWriter, r *http.Request) {
	q, ok := fxQuery(w, r)
	if !ok {
		return
	}
	out, err := h.fx().ExplainDerivedCrossRate(r.Context(), q)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// The production service offers exchange rates; if it ever stops, the routes would silently vanish.
var _ FXService = (*registry.Service)(nil)
