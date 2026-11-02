package handler

// HTTP surface for ORG-04 ownership and control relationships.
//
//	POST /v1/ownership-relationships                    ProposeOwnershipRelationship  (201)
//	GET  /v1/ownership-relationships/{relationshipID}   one relationship
//	POST /v1/ownership-relationships/{relationshipID}/approval   ApproveOwnershipRelationship
//	POST /v1/ownership-relationships/{relationshipID}/rejection  RejectOwnershipRelationship
//
// A relationship is proposed, then decided by a DIFFERENT principal. The decider
// is the verified caller; no body field names it.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
)

// OwnershipService is the ORG-04 half of the service contract.
type OwnershipService interface {
	ProposeOwnershipRelationship(ctx context.Context, req domain.ProposeOwnershipRequest) (*domain.OwnershipRelationship, error)
	ApproveOwnershipRelationship(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error)
	RejectOwnershipRelationship(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error)
	GetOwnershipRelationship(ctx context.Context, relationshipID string) (*domain.OwnershipRelationship, error)
}

func registerOwnershipRoutes(r chi.Router, h *Handler) {
	r.Post("/ownership-relationships", h.ProposeOwnershipRelationship)
	r.Get("/ownership-relationships/{relationshipID}", h.GetOwnershipRelationship)
	r.Post("/ownership-relationships/{relationshipID}/approval", h.ApproveOwnershipRelationship)
	r.Post("/ownership-relationships/{relationshipID}/rejection", h.RejectOwnershipRelationship)
}

// ProposeOwnershipRelationship files a PROPOSED relationship.
func (h *Handler) ProposeOwnershipRelationship(w http.ResponseWriter, r *http.Request) {
	var req domain.ProposeOwnershipRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	rel, err := h.svc.ProposeOwnershipRelationship(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

// GetOwnershipRelationship returns one relationship of the caller's tenant.
func (h *Handler) GetOwnershipRelationship(w http.ResponseWriter, r *http.Request) {
	rel, err := h.svc.GetOwnershipRelationship(r.Context(), chi.URLParam(r, "relationshipID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// ApproveOwnershipRelationship activates a PROPOSED relationship.
func (h *Handler) ApproveOwnershipRelationship(w http.ResponseWriter, r *http.Request) {
	h.decideOwnership(w, r, h.svc.ApproveOwnershipRelationship)
}

// RejectOwnershipRelationship refuses a PROPOSED relationship.
func (h *Handler) RejectOwnershipRelationship(w http.ResponseWriter, r *http.Request) {
	h.decideOwnership(w, r, h.svc.RejectOwnershipRelationship)
}

func (h *Handler) decideOwnership(w http.ResponseWriter, r *http.Request,
	decide func(context.Context, string, domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error)) {
	var req domain.OwnershipDecisionRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	rel, err := decide(r.Context(), chi.URLParam(r, "relationshipID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}
