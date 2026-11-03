package handler

// HTTP surface for ORG-04 ownership and control relationships.
//
//	POST /v1/ownership-relationships                    ProposeOwnershipRelationship  (201)
//	GET  /v1/ownership-relationships/{relationshipID}   one relationship
//	POST /v1/ownership-relationships/{relationshipID}/approval   ApproveOwnershipRelationship
//	POST /v1/ownership-relationships/{relationshipID}/rejection  RejectOwnershipRelationship
//	POST /v1/ownership-relationships/{relationshipID}/supersession  SupersedeRelationship (201, a new PROPOSED row)
//	POST /v1/ownership-relationships/{relationshipID}/end        EndRelationship
//
// A relationship is proposed, then decided by a DIFFERENT principal. The decider
// is the verified caller; no body field names it.

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// OwnershipService is the ORG-04 half of the service contract.
type OwnershipService interface {
	ProposeOwnershipRelationship(ctx context.Context, req domain.ProposeOwnershipRequest) (*domain.OwnershipRelationship, error)
	ApproveOwnershipRelationship(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error)
	RejectOwnershipRelationship(ctx context.Context, relationshipID string, req domain.OwnershipDecisionRequest) (*domain.OwnershipRelationship, error)
	SupersedeOwnershipRelationship(ctx context.Context, relationshipID string, req domain.SupersedeOwnershipRequest) (*domain.OwnershipRelationship, error)
	EndOwnershipRelationship(ctx context.Context, relationshipID string, req domain.EndOwnershipRequest) (*domain.OwnershipRelationship, error)
	GetOwnershipGraph(ctx context.Context, asOf time.Time, root string) (*domain.OwnershipGraphView, error)
	GetGroupAsOf(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.GroupView, error)
	ListEntityParents(ctx context.Context, legalEntityID string, asOf time.Time) (*registry.EntityParents, error)
	ExplainControlChain(ctx context.Context, legalEntityID string, asOf time.Time) (*domain.ControlChainView, error)
	GetOwnershipRelationship(ctx context.Context, relationshipID string) (*domain.OwnershipRelationship, error)
}

func registerOwnershipRoutes(r chi.Router, h *Handler) {
	r.Post("/ownership-relationships", h.ProposeOwnershipRelationship)
	r.Get("/ownership-relationships/{relationshipID}", h.GetOwnershipRelationship)
	r.Post("/ownership-relationships/{relationshipID}/approval", h.ApproveOwnershipRelationship)
	r.Post("/ownership-relationships/{relationshipID}/rejection", h.RejectOwnershipRelationship)
	r.Post("/ownership-relationships/{relationshipID}/supersession", h.SupersedeOwnershipRelationship)
	r.Post("/ownership-relationships/{relationshipID}/end", h.EndOwnershipRelationship)
	r.Get("/ownership-graph", h.GetOwnershipGraph)
	r.Get("/entities/{entityID}/group", h.GetGroupAsOf)
	r.Get("/entities/{entityID}/parents", h.ListEntityParents)
	r.Get("/entities/{entityID}/control-chain", h.ExplainControlChain)
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

// SupersedeOwnershipRelationship proposes new terms for an ACTIVE relationship;
// the replacement is a new PROPOSED row, decided like any other.
func (h *Handler) SupersedeOwnershipRelationship(w http.ResponseWriter, r *http.Request) {
	var req domain.SupersedeOwnershipRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	rel, err := h.svc.SupersedeOwnershipRelationship(r.Context(), chi.URLParam(r, "relationshipID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, rel)
}

// EndOwnershipRelationship records that an ACTIVE relationship stopped.
func (h *Handler) EndOwnershipRelationship(w http.ResponseWriter, r *http.Request) {
	var req domain.EndOwnershipRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	rel, err := h.svc.EndOwnershipRelationship(r.Context(), chi.URLParam(r, "relationshipID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rel)
}

// asOfParam reads the optional as_of query parameter (RFC3339 or YYYY-MM-DD);
// absent means now. A malformed value is answered 400 and ok is false.
func asOfParam(w http.ResponseWriter, r *http.Request) (time.Time, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("as_of"))
	if raw == "" {
		return time.Now().UTC(), true
	}
	t, err := parseAsOf(raw)
	if err != nil {
		writeErrJSON(w, r, http.StatusBadRequest,
			"as_of must be an RFC3339 timestamp or a YYYY-MM-DD date", correlationID(r))
		return time.Time{}, false
	}
	return t, true
}

// GetOwnershipGraph returns the group structure in force at as_of, optionally
// limited to what root_entity_id owns or controls.
func (h *Handler) GetOwnershipGraph(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	view, err := h.svc.GetOwnershipGraph(r.Context(), asOf, strings.TrimSpace(r.URL.Query().Get("root_entity_id")))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// GetGroupAsOf returns the corporate group an entity belonged to at as_of.
func (h *Handler) GetGroupAsOf(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	view, err := h.svc.GetGroupAsOf(r.Context(), chi.URLParam(r, "entityID"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// ListEntityParents returns an entity's direct owners and controllers at as_of.
func (h *Handler) ListEntityParents(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	out, err := h.svc.ListEntityParents(r.Context(), chi.URLParam(r, "entityID"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ExplainControlChain explains who owns and controls an entity at as_of.
func (h *Handler) ExplainControlChain(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	view, err := h.svc.ExplainControlChain(r.Context(), chi.URLParam(r, "entityID"), asOf)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}
