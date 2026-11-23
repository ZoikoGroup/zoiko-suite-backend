package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/org-structure-svc/internal/domain"
)

// ORG-05 maker-checker (ZS-SVC-I-001 s4.5): a reparent that carries financial
// mappings is proposed by one principal and approved by another.

// ProposedChange is the answer to a reparent that needs approval: nothing has
// moved yet; the request and what the move reaches are returned instead.
type ProposedChange struct {
	Status        string                `json:"status"` // PENDING_APPROVAL
	ChangeRequest *domain.ChangeRequest `json:"change_request"`
	Impact        domain.ImpactReport   `json:"impact"`
}

// DecidedChange is the answer to an approval.
type DecidedChange struct {
	ChangeRequest *domain.ChangeRequest `json:"change_request"`
	Unit          *domain.OrgUnit       `json:"unit,omitempty"`
}

// proposeReparent stores the move for approval. 202 means a new request was
// opened; 200 means the same move was already awaiting a decision.
func (h *Handler) proposeReparent(w http.ResponseWriter, r *http.Request, unitID string, req *domain.ReparentOrgUnitRequest, version int64, principalID string, impact domain.ImpactReport) {
	cr, created, err := h.store.CreateReparentRequest(r.Context(), unitID, req, version, principalID, impact)
	if err != nil {
		h.writeOrgUnitErr(w, err, "propose reparent")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, ProposedChange{Status: "PENDING_APPROVAL", ChangeRequest: cr, Impact: impact})
}

// GetReparentImpact previews what a reparent would reach, and whether it would
// need approval, without changing anything.
func (h *Handler) GetReparentImpact(w http.ResponseWriter, r *http.Request) {
	effectiveAt := r.URL.Query().Get("effective_at")
	if !validDate(effectiveAt) {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be a date, YYYY-MM-DD")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	unitID := chi.URLParam(r, "id")
	unit, err := h.store.GetOrgUnit(r.Context(), unitID, "")
	if err != nil {
		h.writeOrgUnitErr(w, err, "reparent impact")
		return
	}
	// The preview names the mapped units beneath this one, so it needs the same
	// right as proposing the move.
	if err := h.authz.CheckAllowed(r.Context(), principalID, unit.LegalEntityID, actionOrgUnitReparent); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	impact, err := h.store.ReparentImpact(r.Context(), unitID, effectiveAt)
	if err != nil {
		h.writeOrgUnitErr(w, err, "reparent impact")
		return
	}
	writeJSON(w, http.StatusOK, impact)
}

func (h *Handler) GetChangeRequest(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	cr, err := h.store.GetChangeRequest(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeOrgUnitErr(w, err, "get change request")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, cr.LegalEntityID, actionOrgUnitView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cr)
}

func (h *Handler) ListChangeRequests(w http.ResponseWriter, r *http.Request) {
	legalEntityID, status := r.URL.Query().Get("legal_entity_id"), r.URL.Query().Get("status")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	switch status {
	case "", domain.ChangePending, domain.ChangeApproved, domain.ChangeRejected, domain.ChangeFailed:
	default:
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be PENDING, APPROVED, REJECTED or FAILED")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionOrgUnitView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListChangeRequests(r.Context(), legalEntityID, status)
	if err != nil {
		h.writeOrgUnitErr(w, err, "list change requests")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ApproveChangeRequest applies a pending change on behalf of an independent
// approver. The person who proposed it is refused (403 sod_denied). If the
// change can no longer be applied as written, the request is closed as FAILED
// and the reason is returned.
func (h *Handler) ApproveChangeRequest(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	cr, err := h.store.GetChangeRequest(r.Context(), id)
	if err != nil {
		h.writeOrgUnitErr(w, err, "approve")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, cr.LegalEntityID, actionOrgUnitApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	decided, unit, err := h.store.ApproveChangeRequest(r.Context(), id, principalID)
	if err != nil {
		h.writeOrgUnitErr(w, err, "approve")
		return
	}

	writeJSON(w, http.StatusOK, DecidedChange{ChangeRequest: decided, Unit: unit})
}

// RejectChangeRequest closes a pending change without applying it.
func (h *Handler) RejectChangeRequest(w http.ResponseWriter, r *http.Request) {
	var body domain.DecideChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(body.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	cr, err := h.store.GetChangeRequest(r.Context(), id)
	if err != nil {
		h.writeOrgUnitErr(w, err, "reject")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, cr.LegalEntityID, actionOrgUnitApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	rejected, err := h.store.RejectChangeRequest(r.Context(), id, principalID, body.Reason)
	if err != nil {
		h.writeOrgUnitErr(w, err, "reject")
		return
	}
	writeJSON(w, http.StatusOK, rejected)
}
