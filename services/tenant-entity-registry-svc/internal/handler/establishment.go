package handler

// HTTP surface for ORG-08 establishments.
//
//	POST /v1/establishments                                  CreateEstablishment (201, 200 replay)
//	GET  /v1/establishments/{id}?as_of=&history=true         GetEstablishment
//	POST /v1/establishments/{id}:activate                    ChangeEstablishmentStatus to ACTIVE
//	POST /v1/establishments/{id}:deactivate                  ChangeEstablishmentStatus to INACTIVE
//	POST /v1/establishments/{id}:close                       ChangeEstablishmentStatus to CLOSED
//	GET  /v1/entities/{entityID}/establishments?status=&as_of=   ListEntityEstablishments, or GetEstablishmentsAsOf when as_of is given
//	GET  /v1/entities/{entityID}/addresses?as_of=&include_ended=true   the entity's addresses, the registered office among them
//
// The status changes are named commands, not a status write (ST-3). Mounted only when the service satisfies
// EstablishmentService, which *registry.Service does.

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// EstablishmentService is the ORG-08 establishment half of the service contract.
type EstablishmentService interface {
	CreateEstablishment(ctx context.Context, req domain.CreateEstablishmentRequest) (*registry.EstablishmentOutcome, error)
	ChangeEstablishmentStatus(ctx context.Context, establishmentID string, req domain.ChangeEstablishmentStatusRequest) (*registry.EstablishmentOutcome, error)
	GetEstablishment(ctx context.Context, establishmentID string, asOf time.Time, history bool) (*domain.EstablishmentView, error)
	ListEntityEstablishments(ctx context.Context, legalEntityID string, status domain.EstablishmentStatus) ([]domain.Establishment, error)
	GetEstablishmentsAsOf(ctx context.Context, legalEntityID string, asOf time.Time) ([]domain.EstablishmentView, error)
	ListEntityAddresses(ctx context.Context, legalEntityID string, asOf time.Time, includeEnded bool) ([]domain.EntityAddress, error)
}

func registerEstablishmentRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(EstablishmentService); !ok {
		return
	}
	r.Post("/establishments", h.CreateEstablishment)
	r.Get("/establishments/{establishmentID}", h.GetEstablishment)
	r.Post("/establishments/{establishmentID}:activate", h.statusCommand(domain.EstActive))
	r.Post("/establishments/{establishmentID}:deactivate", h.statusCommand(domain.EstInactive))
	r.Post("/establishments/{establishmentID}:close", h.statusCommand(domain.EstClosed))
	r.Get("/entities/{entityID}/establishments", h.ListEntityEstablishments)
	r.Get("/entities/{entityID}/addresses", h.ListEntityAddresses)
}

func (h *Handler) establishments() EstablishmentService {
	es, _ := h.svc.(EstablishmentService)
	return es
}

// CreateEstablishment records a PROPOSED establishment.
func (h *Handler) CreateEstablishment(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateEstablishmentRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.establishments().CreateEstablishment(r.Context(), req)
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

// statusCommand is one named status change.
func (h *Handler) statusCommand(to domain.EstablishmentStatus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req domain.ChangeEstablishmentStatusRequest
		if !decode(w, r, &req) {
			return
		}
		req.To = to
		if req.CorrelationID == "" {
			req.CorrelationID = correlationID(r)
		}
		out, err := h.establishments().ChangeEstablishmentStatus(r.Context(), chi.URLParam(r, "establishmentID"), req)
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// GetEstablishment returns an establishment with the address version in force at as_of.
func (h *Handler) GetEstablishment(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	v, err := h.establishments().GetEstablishment(r.Context(), chi.URLParam(r, "establishmentID"), asOf, r.URL.Query().Get("history") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ListEntityEstablishments lists by current status, or reconstructs the active set when as_of is given.
func (h *Handler) ListEntityEstablishments(w http.ResponseWriter, r *http.Request) {
	entityID := chi.URLParam(r, "entityID")
	if r.URL.Query().Get("as_of") != "" {
		asOf, ok := asOfParam(w, r)
		if !ok {
			return
		}
		out, err := h.establishments().GetEstablishmentsAsOf(r.Context(), entityID, asOf)
		if err != nil {
			h.writeErr(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"as_of": asOf, "items": out})
		return
	}
	out, err := h.establishments().ListEntityEstablishments(r.Context(), entityID, domain.EstablishmentStatus(r.URL.Query().Get("status")))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// ListEntityAddresses lists an entity's addresses.
func (h *Handler) ListEntityAddresses(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.establishments().ListEntityAddresses(r.Context(), chi.URLParam(r, "entityID"), asOf, r.URL.Query().Get("include_ended") == "true")
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// The production service offers establishments; if it ever stops, the routes would silently vanish.
var _ EstablishmentService = (*registry.Service)(nil)
