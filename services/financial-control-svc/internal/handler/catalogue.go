package handler

import (
	"errors"
	"net/http"

	"zoiko.io/financial-control-svc/internal/catalogue"
	"zoiko.io/financial-control-svc/internal/domain"
)

type seedRequest struct {
	Wave int `json:"wave"`
	catalogue.Wave2Options
	catalogue.Wave4Options
}

type seedResult struct {
	ControlCode         string `json:"control_code"`
	Status              string `json:"status"` // created | exists
	ControlDefinitionID string `json:"control_definition_id,omitempty"`
}

// SeedCatalogue — POST /controls/v1/catalogue/seed. Creates the ZS-CONTROL-001 §29
// catalogue definitions for a wave. Idempotent: an existing control code is
// reported "exists" and left untouched (definitions are immutable). Every
// created definition carries an UNAPPROVED rule version — seeding grants no one
// the ability to run a control; an independent approver must still approve it.
func (h *Handler) SeedCatalogue(w http.ResponseWriter, r *http.Request) {
	tenantID, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req seedRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var defs []domain.CreateControlDefinitionRequest
	var err error
	switch req.Wave {
	case 2:
		defs, err = catalogue.Wave2(req.Wave2Options)
	case 3:
		defs, err = catalogue.Wave3(req.EffectiveFrom)
	case 4:
		defs, err = catalogue.Wave4(req.EffectiveFrom, req.Wave4Options)
	case 5:
		defs, err = catalogue.Wave5(req.EffectiveFrom)
	case 6:
		defs, err = catalogue.Wave6(req.EffectiveFrom)
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "wave must be 2, 3, 4, 5 or 6 in this build")
		return
	}
	if err != nil {
		h.writeErr(w, "seed catalogue", err)
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionDefine) {
		return
	}
	results := make([]seedResult, 0, len(defs))
	for _, d := range defs {
		created, err := h.store.CreateDefinition(r.Context(), tenantID, principal, corrID(r), d)
		switch {
		case err == nil:
			results = append(results, seedResult{ControlCode: d.ControlCode, Status: "created", ControlDefinitionID: created.ControlDefinitionID})
		case errors.Is(err, domain.ErrDuplicate):
			results = append(results, seedResult{ControlCode: d.ControlCode, Status: "exists"})
		default:
			h.writeErr(w, "seed catalogue "+d.ControlCode, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"wave": req.Wave, "controls": results})
}

// CatalogueStatus — GET /controls/v1/catalogue/status. For every control in the
// §29 catalogue: implemented, blocked (with the exact missing capability) or not
// started. It is static build information, so it needs read authority only.
func (h *Handler) CatalogueStatus(w http.ResponseWriter, r *http.Request) {
	_, principal, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principal, platformEntity, ActionRead) {
		return
	}
	items := catalogue.Status()
	writeJSON(w, http.StatusOK, map[string]any{"summary": catalogue.Summary(items), "controls": items})
}
