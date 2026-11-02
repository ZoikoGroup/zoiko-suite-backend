package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
)

// HierarchyResponse is the operating hierarchy of one legal entity.
type HierarchyResponse struct {
	LegalEntityID   string                `json:"legal_entity_id"`
	AsOf            string                `json:"as_of"`
	RecordedAsOf    string                `json:"recorded_as_of,omitempty"`
	IncludeInactive bool                  `json:"include_inactive"`
	Roots           []*domain.OrgUnitNode `json:"roots"`
}

// GetHierarchy serves ListHierarchy (no dates: today, as known now) and
// GetHierarchyAsOf (as_of and/or recorded_as_of).
//
//	as_of          the date the structure applied (YYYY-MM-DD)
//	recorded_as_of how much of it had been recorded by then (RFC 3339), so a past
//	               report can be reproduced as it was produced
//	include_inactive  retired units; defaults to false for a current read and true
//	               for a dated one, because a past tree had the units that are
//	               retired today. Unit status is the current status.
func (h *Handler) GetHierarchy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	legalEntityID, asOf, recordedAsOf := q.Get("legal_entity_id"), q.Get("as_of"), q.Get("recorded_as_of")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if asOf != "" && !validDate(asOf) {
		writeError(w, http.StatusBadRequest, "invalid_as_of", "as_of must be a date, YYYY-MM-DD")
		return
	}
	if recordedAsOf != "" {
		if _, err := time.Parse(time.RFC3339, recordedAsOf); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_recorded_as_of", "recorded_as_of must be an RFC 3339 timestamp")
			return
		}
	}
	includeInactive := asOf != "" || recordedAsOf != ""
	if raw := q.Get("include_inactive"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_include_inactive", "include_inactive must be true or false")
			return
		}
		includeInactive = v
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionOrgUnitView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	units, err := h.store.HierarchySnapshot(r.Context(), legalEntityID, asOf, recordedAsOf, includeInactive)
	if err != nil {
		h.writeOrgUnitErr(w, err, "hierarchy")
		return
	}
	roots, err := domain.BuildOrgUnitTree(units)
	if err != nil {
		if errors.Is(err, domain.ErrHierarchyLoop) {
			// Stored data contains a cycle: a defect to surface, not to hide.
			h.log.Error("hierarchy snapshot contains a cycle", zap.String("legal_entity_id", legalEntityID))
			writeError(w, http.StatusInternalServerError, "hierarchy_inconsistent", "the stored hierarchy contains a cycle")
			return
		}
		h.writeOrgUnitErr(w, err, "hierarchy")
		return
	}

	if asOf == "" {
		asOf = time.Now().UTC().Format("2006-01-02")
	}
	writeJSON(w, http.StatusOK, HierarchyResponse{
		LegalEntityID: legalEntityID, AsOf: asOf, RecordedAsOf: recordedAsOf,
		IncludeInactive: includeInactive, Roots: roots,
	})
}
