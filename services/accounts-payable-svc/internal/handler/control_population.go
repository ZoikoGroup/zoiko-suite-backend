package handler

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/accounts-payable-svc/internal/domain"
)

const (
	defaultPopulationLimit = 1000
	maxPopulationLimit     = 10000
)

// GET /v1/control-populations/{population} — docs/architecture/control-population-contract.md.
// Read-only. Unknown query parameters are refused so a misspelt filter can
// never silently widen or narrow the population.
func (h *Handler) ControlPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if chi.URLParam(r, "population") != domain.PopulationOpenInvoices {
		writeError(w, http.StatusNotFound, "population_not_found", "")
		return
	}

	q := r.URL.Query()
	for k := range q {
		switch k {
		case "legal_entity_id", "period_id", "limit", "cursor":
		default:
			writeError(w, http.StatusBadRequest, "unknown_parameter", "query parameter "+strconv.Quote(k)+" is not defined for this population")
			return
		}
	}
	legalEntityID := q.Get("legal_entity_id")
	if !isUUID(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required and must be a UUID")
		return
	}
	cq := domain.ControlPopulationQuery{TenantID: tenantID, LegalEntityID: legalEntityID, Limit: defaultPopulationLimit}
	if p := q.Get("period_id"); p != "" {
		start, err := time.Parse("2006-01", p)
		if err != nil || len(p) != 7 {
			writeError(w, http.StatusBadRequest, "invalid_field", "period_id must be YYYY-MM")
			return
		}
		cq.PeriodEnd = start.AddDate(0, 1, -1).Format("2006-01-02")
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > maxPopulationLimit {
			writeError(w, http.StatusBadRequest, "invalid_field", "limit must be an integer between 1 and "+strconv.Itoa(maxPopulationLimit))
			return
		}
		cq.Limit = n
	}
	if c := q.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || !isUUID(string(raw)) {
			writeError(w, http.StatusBadRequest, "invalid_field", domain.ErrInvalidCursor.Error())
			return
		}
		cq.AfterRecordID = string(raw)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionReadControlPopulation); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.ControlPopulation(r.Context(), cq)
	if err != nil {
		if errors.Is(err, domain.ErrPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
			return
		}
		if errors.Is(err, domain.ErrInvalidIdentifier) {
			writeError(w, http.StatusBadRequest, "invalid_field", "tenant scope must be a UUID")
			return
		}
		h.log.Error("ControlPopulation: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	next := ""
	if page.NextRecordID != "" {
		next = base64.RawURLEncoding.EncodeToString([]byte(page.NextRecordID))
	}
	writeJSON(w, http.StatusOK, struct {
		Records        []domain.ControlRecord `json:"records"`
		NextCursor     string                 `json:"next_cursor"`
		Watermark      string                 `json:"watermark"`
		DeclaredTotals domain.DeclaredTotals  `json:"declared_totals"`
	}{page.Records, next, page.Watermark, page.DeclaredTotals})
}
