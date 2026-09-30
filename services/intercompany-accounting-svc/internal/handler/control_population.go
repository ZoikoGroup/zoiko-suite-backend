package handler

// Control-population source endpoint (docs/architecture/control-population-contract.md):
//
//	GET /v1/control-populations/{population}
//
// Read-only. Serves financial-control-svc's pull of the entry-legs population.

import (
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/intercompany-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/intercompany-accounting-svc/internal/middleware"
)

// actionControlPopulationRead authorizes reading a legal entity's intercompany
// entry legs as a control population.
const actionControlPopulationRead = "INTERCOMPANY_CONTROL_POPULATION_READ"

const (
	controlDefaultLimit = 1000
	controlMaxLimit     = 5000
)

// entityIDPattern bounds legal_entity_id. The intercompany_entries entity
// columns are VARCHAR(255) (not UUID), so ids like "le-101" are legitimate.
var entityIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// period_id is deliberately absent: entries carry no period (lifetime population).
var controlAllowedParams = map[string]bool{
	"legal_entity_id": true, "leg": true, "limit": true, "cursor": true,
}

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

	if chi.URLParam(r, "population") != domain.PopulationEntryLegs {
		writeError(w, http.StatusNotFound, "unknown_population", "unknown control population")
		return
	}

	query := r.URL.Query()
	for k, v := range query {
		if !controlAllowedParams[k] {
			writeError(w, http.StatusBadRequest, "unknown_parameter", "unknown query parameter: "+k)
			return
		}
		if len(v) != 1 {
			writeError(w, http.StatusBadRequest, "repeated_parameter", "query parameter must appear once: "+k)
			return
		}
	}

	legalEntityID := query.Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_legal_entity_id", "legal_entity_id is required")
		return
	}
	if !entityIDPattern.MatchString(legalEntityID) {
		writeError(w, http.StatusBadRequest, "invalid_legal_entity_id", "legal_entity_id is invalid")
		return
	}

	leg := query.Get("leg")
	if leg == "" {
		writeError(w, http.StatusBadRequest, "missing_leg", "leg is required (source|target)")
		return
	}
	if leg != domain.LegSource && leg != domain.LegTarget {
		writeError(w, http.StatusBadRequest, "invalid_leg", "leg must be source or target")
		return
	}

	limit := controlDefaultLimit
	if query.Has("limit") {
		n, err := strconv.Atoi(query.Get("limit"))
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		limit = n
		if limit > controlMaxLimit {
			limit = controlMaxLimit
		}
	}

	after := ""
	if query.Has("cursor") {
		raw, err := base64.RawURLEncoding.DecodeString(query.Get("cursor"))
		if err != nil || len(raw) == 0 || len(raw) > 128 || !utf8.Valid(raw) {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		after = string(raw)
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionControlPopulationRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	page, err := h.store.ControlPopulation(r.Context(), domain.ControlPopulationQuery{
		TenantID: tenantID, LegalEntityID: legalEntityID, Leg: leg, AfterRecordID: after, Limit: limit,
	})
	if err != nil {
		if errors.Is(err, domain.ErrPopulationTooLarge) {
			writeError(w, http.StatusUnprocessableEntity, "population_too_large", err.Error())
			return
		}
		h.log.Error("control population read failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "failed to read control population")
		return
	}
	if page.NextCursor != "" {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))
	}
	writeJSON(w, http.StatusOK, page)
}
