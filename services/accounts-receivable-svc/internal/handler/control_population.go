package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-receivable-svc/internal/domain"
)

const (
	populationOpenInvoices = "open-invoices"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

// GetControlPopulation serves GET /v1/control-populations/{population}, the
// read-only source side of the control population contract
// (docs/architecture/control-population-contract.md). Only `open-invoices` exists.
//
// Any query parameter outside {legal_entity_id, period_id, limit, cursor} is
// refused: a misspelt filter must never silently widen or narrow a population.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if chi.URLParam(r, "population") != populationOpenInvoices {
		writeError(w, http.StatusNotFound, "population_not_found", "")
		return
	}

	q := r.URL.Query()
	for name := range q {
		switch name {
		case "legal_entity_id", "period_id", "limit", "cursor":
		default:
			writeError(w, http.StatusBadRequest, "unknown_parameter", fmt.Sprintf("query parameter %q is not defined for this population", name))
			return
		}
	}

	legalEntityID := q.Get("legal_entity_id")
	if legalEntityID == "" || !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required and must be a UUID")
		return
	}

	var cutOff *time.Time
	if raw := q.Get("period_id"); raw != "" {
		month, err := time.Parse("2006-01", raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "period_id must be YYYY-MM")
			return
		}
		end := month.AddDate(0, 1, -1)
		cutOff = &end
	}

	limit := defaultPopulationLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field",
				fmt.Sprintf("limit must be an integer between 1 and %d", maxPopulationLimit))
			return
		}
		limit = n
	}

	after := ""
	if raw := q.Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || !isUUID(string(decoded)) {
			writeError(w, http.StatusBadRequest, "invalid_field", "cursor is not valid")
			return
		}
		after = string(decoded)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.ControlPopulation(r.Context(), domain.ControlPopulationQuery{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		CutOff:        cutOff,
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		if errors.Is(err, domain.ErrPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
			return
		}
		h.log.Error("ControlPopulation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	if page.Records == nil {
		page.Records = []domain.ControlRecord{}
	}
	if page.DeclaredTotals.Totals == nil {
		page.DeclaredTotals.Totals = map[string]string{}
	}
	if page.NextCursor != "" {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))
	}
	writeJSON(w, http.StatusOK, page)
}
