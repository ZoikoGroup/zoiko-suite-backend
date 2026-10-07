package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/inventory-management-svc/internal/domain"
)

const (
	populationStockCountLines = "stock-count-lines"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

// isUUID reports whether s is a canonical 36-character UUID.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// GetControlPopulation serves GET /v1/control-populations/{population}, the
// read-only source side of the control population contract
// (docs/architecture/control-population-contract.md). Only `stock-count-lines`
// exists.
//
// Any query parameter outside {legal_entity_id, count_id, side, limit, cursor},
// and any parameter given more than once, is refused: a misspelt filter must
// never silently widen or narrow a population. period_id is not defined for
// this population (a count has its own cut-off) and is therefore refused too.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if chi.URLParam(r, "population") != populationStockCountLines {
		writeError(w, http.StatusNotFound, "population_not_found", "")
		return
	}

	q := r.URL.Query()
	for name, values := range q {
		switch name {
		case "legal_entity_id", "count_id", "side", "limit", "cursor":
			if len(values) != 1 {
				writeError(w, http.StatusBadRequest, "repeated_parameter", fmt.Sprintf("query parameter %q must be given exactly once", name))
				return
			}
		default:
			writeError(w, http.StatusBadRequest, "unknown_parameter", fmt.Sprintf("query parameter %q is not defined for this population", name))
			return
		}
	}

	legalEntityID := q.Get("legal_entity_id")
	if !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required and must be a UUID")
		return
	}
	countID := q.Get("count_id")
	if !isUUID(countID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "count_id is required and must be a UUID")
		return
	}
	side := q.Get("side")
	if side != domain.ControlSideBook && side != domain.ControlSidePhysical {
		writeError(w, http.StatusBadRequest, "invalid_field", "side is required and must be book or physical")
		return
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

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionInventoryControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.ControlPopulationStockCountLines(r.Context(), domain.StockCountLinesQuery{
		TenantID:      tenantID,
		LegalEntityID: legalEntityID,
		CountID:       countID,
		Side:          side,
		Limit:         limit,
		AfterRecordID: after,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrStockCountNotFound):
			writeError(w, http.StatusNotFound, "stock_count_not_found", "")
		case errors.Is(err, domain.ErrPopulationTooLarge):
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
		default:
			h.log.Error("ControlPopulation: store unavailable", zap.Error(err))
			writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		}
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
