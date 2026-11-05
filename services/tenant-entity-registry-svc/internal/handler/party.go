package handler

// HTTP surface for ORG-07 parties and MDM-01 identity creation.
//
//	POST /v1/parties                                  CreateParty (201 created, 200 existing or replay, 202 REVIEW_REQUIRED)
//	GET  /v1/parties?q=&kind=&role=&status=           SearchParties
//	GET  /v1/parties/{id}?as_of=&known_at=&reveal=    GetParty / GetPartyAsOf
//	POST /v1/parties/{id}/facts:propose               AmendPartyProfile
//	POST /v1/parties/{id}/roles                       AddPartyRole
//	POST /v1/parties/{id}/roles/{roleID}/removal      RemovePartyRole
//	POST /v1/parties/{id}/identifiers                 LinkExternalIdentifier (201, 200 already linked, 202 conflict case)
//	GET  /v1/identifiers:find?scheme=&scope=&value=   FindByExternalIdentifier
//	GET  /v1/resolution-cases/{id}                    one stewardship case
//
// The party routes are mounted only when the service satisfies PartyService, which *registry.Service does; the
// large Service interface, and every stub of it, is left alone.

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

// PartyService is the ORG-07 half of the service contract.
type PartyService interface {
	CreateParty(ctx context.Context, req domain.CreatePartyRequest) (*domain.CreatePartyResult, error)
	GetParty(ctx context.Context, partyID string, asOf, knownAt time.Time, reveal bool) (*domain.PartyView, error)
	ProposePartyFact(ctx context.Context, partyID string, req domain.ProposeFactRequest) (*registry.PartyFactOutcome, error)
	AddPartyRole(ctx context.Context, partyID string, req domain.AddPartyRoleRequest) (*registry.PartyRoleOutcome, error)
	RemovePartyRole(ctx context.Context, partyID, roleID string, req domain.RemovePartyRoleRequest) (*registry.PartyRoleOutcome, error)
	LinkPartyIdentifier(ctx context.Context, partyID string, req domain.LinkIdentifierRequest) (*registry.PartyIdentifierOutcome, error)
	FindPartiesByIdentifier(ctx context.Context, scheme domain.IdentifierScheme, scope, value string) ([]domain.PartySummary, error)
	SearchParties(ctx context.Context, q domain.PartySearchQuery) ([]domain.PartySummary, string, error)
	GetResolutionCase(ctx context.Context, caseID string) (*domain.ResolutionCase, error)
}

func registerPartyRoutes(r chi.Router, h *Handler) {
	if _, ok := h.svc.(PartyService); !ok {
		return
	}
	r.Post("/parties", h.CreateParty)
	r.Get("/parties", h.SearchParties)
	r.Get("/parties/{partyID}", h.GetParty)
	r.Post("/parties/{partyID}/facts:propose", h.ProposePartyFact)
	r.Post("/parties/{partyID}/roles", h.AddPartyRole)
	r.Post("/parties/{partyID}/roles/{roleID}/removal", h.RemovePartyRole)
	r.Post("/parties/{partyID}/identifiers", h.LinkPartyIdentifier)
	r.Get("/identifiers:find", h.FindByExternalIdentifier)
	r.Get("/resolution-cases/{caseID}", h.GetResolutionCase)
	registerPartyResolutionRoutes(r, h)
	registerAddressRoutes(r, h)
	registerEstablishmentRoutes(r, h)
	registerRegistrationRoutes(r, h)
	registerIdentifierVerificationRoutes(r, h)
}

func (h *Handler) parties() PartyService {
	ps, _ := h.svc.(PartyService)
	return ps
}

// CreateParty runs the create flow.
func (h *Handler) CreateParty(w http.ResponseWriter, r *http.Request) {
	var req domain.CreatePartyRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	res, err := h.parties().CreateParty(r.Context(), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusOK
	switch {
	case res.Outcome == domain.OutcomeCreated && !res.Replayed:
		status = http.StatusCreated
	case res.Outcome == domain.OutcomeReviewRequired:
		status = http.StatusAccepted
	}
	writeJSON(w, status, res)
}

// GetParty returns the master view in force at as_of as known at known_at.
func (h *Handler) GetParty(w http.ResponseWriter, r *http.Request) {
	asOf, ok := asOfParam(w, r)
	if !ok {
		return
	}
	var knownAt time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("known_at")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeErrJSON(w, r, http.StatusBadRequest, "known_at must be an RFC3339 timestamp", correlationID(r))
			return
		}
		knownAt = t.UTC()
	}
	reveal := r.URL.Query().Get("reveal") == "true"
	v, err := h.parties().GetParty(r.Context(), chi.URLParam(r, "partyID"), asOf, knownAt, reveal)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ProposePartyFact records a sourced fact without overwriting a verified one.
func (h *Handler) ProposePartyFact(w http.ResponseWriter, r *http.Request) {
	var req domain.ProposeFactRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.parties().ProposePartyFact(r.Context(), chi.URLParam(r, "partyID"), req)
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

// AddPartyRole gives a party a role.
func (h *Handler) AddPartyRole(w http.ResponseWriter, r *http.Request) {
	var req domain.AddPartyRoleRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.parties().AddPartyRole(r.Context(), chi.URLParam(r, "partyID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// RemovePartyRole ends a role.
func (h *Handler) RemovePartyRole(w http.ResponseWriter, r *http.Request) {
	var req domain.RemovePartyRoleRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.parties().RemovePartyRole(r.Context(), chi.URLParam(r, "partyID"), chi.URLParam(r, "roleID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// LinkPartyIdentifier links an external identifier.
func (h *Handler) LinkPartyIdentifier(w http.ResponseWriter, r *http.Request) {
	var req domain.LinkIdentifierRequest
	if !decode(w, r, &req) {
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = correlationID(r)
	}
	out, err := h.parties().LinkPartyIdentifier(r.Context(), chi.URLParam(r, "partyID"), req)
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	status := http.StatusCreated
	switch {
	case out.Case != nil:
		status = http.StatusAccepted
	case out.NoOp:
		status = http.StatusOK
	}
	writeJSON(w, status, out)
}

// FindByExternalIdentifier finds the party an identifier names.
func (h *Handler) FindByExternalIdentifier(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := h.parties().FindPartiesByIdentifier(r.Context(), domain.IdentifierScheme(q.Get("scheme")), q.Get("scope"), q.Get("value"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// SearchParties searches the authoritative tables.
func (h *Handler) SearchParties(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 200 {
			writeErrJSON(w, r, http.StatusBadRequest, "limit must be an integer from 1 to 200", correlationID(r))
			return
		}
		limit = n
	}
	out, next, err := h.parties().SearchParties(r.Context(), domain.PartySearchQuery{
		Text: q.Get("q"), Kind: domain.PartyKind(q.Get("kind")), Role: domain.RoleType(q.Get("role")),
		Status: domain.PartyStatus(q.Get("status")), Limit: limit, After: q.Get("cursor"),
	})
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "next_cursor": next})
}

// GetResolutionCase reads one stewardship case.
func (h *Handler) GetResolutionCase(w http.ResponseWriter, r *http.Request) {
	c, err := h.parties().GetResolutionCase(r.Context(), chi.URLParam(r, "caseID"))
	if err != nil {
		h.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// The production service offers parties; if it ever stops, the routes would silently vanish.
var _ PartyService = (*registry.Service)(nil)
