package handler

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	svcmiddleware "zoiko.io/payroll-run-svc/internal/middleware"
)

const (
	// actionControlPopulationRead is the authorization-svc action a caller needs
	// (for the requested legal entity) to extract a payroll control population.
	actionControlPopulationRead = "PAYROLL_CONTROL_POPULATION_READ"

	defaultPopulationLimit = 1000
	maxPopulationLimit     = 5000
)

// legalEntityIDPattern accepts a UUID as well as this service's other entity ids
// (payroll_runs.legal_entity_id is VARCHAR, e.g. "le-us").
var legalEntityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// GetControlPopulation serves GET /v1/control-populations/{population}, the
// read-only source side of the control population contract
// (docs/architecture/control-population-contract.md) for `pay-slips` and
// `payroll-runs`.
//
// This is personal payroll data: the response identifies an employee by
// employee_number only. No name, contact or bank detail is ever selected.
//
// Any query parameter outside {legal_entity_id, period_id, measure, limit,
// cursor}, or given more than once, is refused.
func (h *Handler) GetControlPopulation(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	population := chi.URLParam(r, "population")
	if population != domain.PopulationPaySlips && population != domain.PopulationPayrollRuns {
		writeError(w, http.StatusNotFound, "population_not_found", "")
		return
	}

	q := r.URL.Query()
	for name, vals := range q {
		switch name {
		case "legal_entity_id", "period_id", "measure", "limit", "cursor":
		default:
			writeError(w, http.StatusBadRequest, "unknown_parameter", fmt.Sprintf("query parameter %q is not defined for this population", name))
			return
		}
		if len(vals) > 1 {
			writeError(w, http.StatusBadRequest, "repeated_parameter", fmt.Sprintf("query parameter %q must not be repeated", name))
			return
		}
	}

	legalEntityID := q.Get("legal_entity_id")
	if !legalEntityIDPattern.MatchString(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is required and must be a valid entity id")
		return
	}

	measure := q.Get("measure")
	if measure != domain.MeasureGross && measure != domain.MeasureNet {
		writeError(w, http.StatusBadRequest, "invalid_field", "measure is required and must be gross or net")
		return
	}

	// Flow semantics: the pay date must fall in this calendar month exactly.
	var periodStart *time.Time
	if raw := q.Get("period_id"); raw != "" {
		month, err := time.Parse("2006-01", raw)
		if err != nil || len(raw) != 7 {
			writeError(w, http.StatusBadRequest, "invalid_field", "period_id must be YYYY-MM")
			return
		}
		periodStart = &month
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
		if err != nil || !uuidPattern.MatchString(string(decoded)) {
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
		Population:    population,
		Measure:       measure,
		PeriodStart:   periodStart,
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
