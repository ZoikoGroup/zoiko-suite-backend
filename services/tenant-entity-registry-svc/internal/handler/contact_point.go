package handler

// HTTP surface for MDM-01 party contact points.
//
//	POST /v1/parties/{partyID}/contact-points                    CreateContactPoint (201): PROPOSED
//	POST /v1/party-contact-points/{contactPointID}:verify        VerifyContactPoint: maker-checker, evidence_ref required
//	POST /v1/party-contact-points/{contactPointID}:retire        RetireContactPoint: reason required, final
//	GET  /v1/parties/{partyID}/contact-points?channel=&status=&as_of=   the contact points in force at as_of, addresses included
//
// The address is returned only by the read, which needs PARTY_CONTACT_READ. It is never logged and never in an event.
// Mounted only when the service satisfies ContactPointService, which *registry.Service does.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// ContactPointService is the MDM-01 contact point half of the service contract.
type ContactPointService interface {
	CreateContactPoint(ctx context.Context, partyID string, req domain.CreateContactPointRequest) (*registry.ContactPointOutcome, error)
	VerifyContactPoint(ctx context.Context, id string, req domain.VerifyContactPointRequest) (*registry.ContactPointOutcome, error)
	RetireContactPoint(ctx context.Context, id string, req domain.RetireContactPointRequest) (*registry.ContactPointOutcome, error)
	ListPartyContactPoints(ctx context.Context, partyID string, q domain.ContactPointQuery) ([]domain.ContactPointItem, error)
}

func registerContactPointRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(ContactPointService); !ok {
		return
	}
	r.Post("/parties/{partyID}/contact-points", h.CreateContactPoint)
	r.Get("/parties/{partyID}/contact-points", h.ListPartyContactPoints)
	r.Post("/party-contact-points/{contactPointID}:verify", h.VerifyContactPoint)
	r.Post("/party-contact-points/{contactPointID}:retire", h.RetireContactPoint)
}

func (h *Handler) contactPoints() ContactPointService {
	cs, _ := h.svc.(ContactPointService)
	return cs
}

// CreateContactPoint proposes a contact point.
func (h *Handler) CreateContactPoint(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateContactPointRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.contactPoints().CreateContactPoint(r.Context(), chi.URLParam(r, "partyID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// VerifyContactPoint verifies a PROPOSED contact point.
func (h *Handler) VerifyContactPoint(w http.ResponseWriter, r *http.Request) {
	var req domain.VerifyContactPointRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.contactPoints().VerifyContactPoint(r.Context(), chi.URLParam(r, "contactPointID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RetireContactPoint retires a contact point, finally.
func (h *Handler) RetireContactPoint(w http.ResponseWriter, r *http.Request) {
	var req domain.RetireContactPointRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.contactPoints().RetireContactPoint(r.Context(), chi.URLParam(r, "contactPointID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ListPartyContactPoints returns the party's contact points in force at as_of.
func (h *Handler) ListPartyContactPoints(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	q := domain.ContactPointQuery{
		Channel: domain.ContactChannel(r.URL.Query().Get("channel")),
		Status:  domain.ContactStatus(r.URL.Query().Get("status")),
		AsOf:    asOf,
	}
	items, err := h.contactPoints().ListPartyContactPoints(r.Context(), chi.URLParam(r, "partyID"), q)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contact_points": items})
}

// The production service offers contact points; if it ever stops, the routes would silently vanish.
var _ ContactPointService = (*registry.Service)(nil)
